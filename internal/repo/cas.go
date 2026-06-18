// cas.go — content-addressed store: chunk Put/Get with compression + encryption + storage plugins.
package repo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/compression"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/compression/none"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// asArray converts repo.Hash back to [32]byte for the storage plugin's
// PutOptions.ContentSHA256. Hash and [32]byte share an underlying type;
// this is just a typed conversion at the API boundary.
func (h Hash) asArray() [32]byte { return [32]byte(h) }

// CASRetention configures per-Put retention propagation. When
// active, every PutChunk includes a RetainUntil deadline in
// PutOptions, which storage backends with WORM support
// (S3 Object Lock, Azure immutable blob) honour as the deletion
// floor. Backends without WORM ignore the field.
//
// Construct via WithRetention; the empty value disables retention
// propagation (default).
type CASRetention struct {
	// RetainUntil is the absolute deadline propagated to every
	// PutChunk's PutOptions.RetainUntil. Zero disables.
	RetainUntil time.Time
	// Mode is the WORMMode propagated alongside the deadline.
	// Backends use this to choose the lock posture (compliance
	// vs governance). Empty disables, regardless of RetainUntil.
	Mode storage.WORMMode
}

// IsZero reports whether retention is unconfigured.
func (r CASRetention) IsZero() bool {
	return r.RetainUntil.IsZero() || r.Mode == storage.WORMNone
}

// CAS is a content-addressed object store layered on top of a StoragePlugin.
//
// The contract is simple: every chunk lives at a key derived from its
// SHA-256 hash of the PLAINTEXT bytes (so dedup happens before
// compression). On disk each chunk is wrapped in a tiny envelope
// — see internal/plugin/compression — that records which codec
// produced the payload, so multiple backups using different codecs
// can co-exist in one repo and a reader can always recover the
// plaintext.
//
// Layout (chunks/sha256/aa/bb/aabb<rest-of-hex>.chk) splits the hex hash
// into a 2/2/60 directory tree. On filesystem-backed repos this keeps any
// single directory's readdir cheap even at billions-of-chunks scale; on
// object stores the prefix has the same effect for LIST partitioning.
//
// Goroutine safety: methods are safe for concurrent use. The internal
// "already-seen" cache is a sync.Map; the underlying storage plugin must
// also be concurrency-safe (every plugin we ship is).
type CAS struct {
	sp       storage.StoragePlugin
	seen     sync.Map               // Hash -> struct{}
	writer   compression.Compressor // codec used for new Put calls
	registry *compression.CodecRegistry

	// Optional encryption. When encWriter is non-nil, every PutChunk
	// encrypts the (possibly compressed) payload before writing.
	// encRegistry maps an envelope's recorded encryption-algo back
	// to the Encryptor that holds the matching key — at most one
	// Encryptor per algo in v0.1 (a single per-CAS key).
	encWriter   encryption.Encryptor
	encRegistry *encryption.CodecRegistry

	// Optional retention propagation (WORM). When non-zero, every
	// PutChunk includes RetainUntil + RetentionMode in PutOptions.
	retention CASRetention

	// retentionUnenforceable is set at NewCAS time when the operator
	// configured retention but the storage backend doesn't advertise
	// WORM support.  PutChunk refuses with a structured error in
	// that case rather than silently writing chunks the backend will
	// happily delete on demand.  Audit v23 corner case #9.
	//
	// Operators who explicitly accept this gap (test repos pointed
	// at file:// for development) opt out via
	// WithRetentionAllowUnenforced.
	retentionUnenforceable   bool
	retentionAllowUnenforced bool

	// chunkDurability is the storage.Durability passed on every
	// PutChunk. The zero value (DurabilityInline) keeps the legacy
	// behaviour — each chunk is fsync'd before PutChunk returns.
	// WithChunkDurability(DurabilityDeferred) lets a bulk writer
	// (base backup, WAL streamer) batch chunk writes and pay one
	// Barrier for all of them; the caller MUST then call
	// CAS.Barrier before treating the chunks as committed.
	chunkDurability storage.Durability

	// hints is an optional set of plaintext chunk hashes the caller
	// believes are already present in the repo (see WithDedupHints).
	// When a PutChunk hash is hinted and not yet seen this session,
	// PutChunk issues one cheap Stat probe instead of paying the full
	// compress + encrypt + upload for a chunk the repo already holds.
	// nil disables the probe — zero overhead, the right default for a
	// first backup.
	hints map[Hash]struct{}

	// dedup counters — atomics because PutChunk runs concurrently
	// (the base-backup chunk worker pool). Read via DedupStats.
	dedupMiss    atomic.Int64 // chunks newly written to the repo
	dedupInMem   atomic.Int64 // skipped — already written this session
	dedupStorage atomic.Int64 // skipped — confirmed already in the repo

	// seen-cache bound. Without it, `seen` grows one entry per distinct
	// chunk for the whole lifetime of the CAS — a leak on a long-lived
	// instance, most acutely the single CAS a `wal stream` session
	// reuses across every reconnect for days/weeks (memory-leak audit
	// #1). seenCap caps the cache; markSeen clears it wholesale when a
	// fresh insert would exceed the cap (O(1) amortised). Zero disables
	// the bound. Dedup CORRECTNESS never depends on the cache — the
	// IfNotExists Put is the backstop — so a cleared entry only costs one
	// extra existence roundtrip on its next reference.
	seenCap   int
	seenCount atomic.Int64
	seenMu    sync.Mutex
}

// defaultSeenCacheLimit bounds the positive cache on every CAS unless
// overridden via WithSeenCacheLimit. ~1M entries (hash key + struct{}
// value + sync.Map overhead ≈ tens of MB at the ceiling) is far above
// any single backup's working set, so normal backups never evict, while
// a perpetual `wal stream` can no longer grow it without bound.
const defaultSeenCacheLimit = 1 << 20

// DedupStats is a snapshot of a CAS's PutChunk dedup outcomes, counted
// over every PutChunk call since construction. HitsInMemory + HitsStorage
// are the chunks PutChunk did NOT have to compress, encrypt and upload.
type DedupStats struct {
	Misses       int64 `json:"misses"`         // chunks newly written
	HitsInMemory int64 `json:"hits_in_memory"` // already written this session
	HitsStorage  int64 `json:"hits_storage"`   // already present in the repo
}

// Total returns the number of PutChunk calls the stats cover.
func (d DedupStats) Total() int64 { return d.Misses + d.HitsInMemory + d.HitsStorage }

// HitRate is the deduplicated fraction (0..1) of all PutChunk calls —
// 0 when no chunks were put.
func (d DedupStats) HitRate() float64 {
	t := d.Total()
	if t == 0 {
		return 0
	}
	return float64(d.HitsInMemory+d.HitsStorage) / float64(t)
}

// CASOption configures a CAS at construction.
type CASOption func(*CAS)

// WithCompressor sets the codec used for PutChunk. If unset, the CAS
// writes plaintext under an AlgoNone envelope. The registry is
// updated to include the codec for read-back.
func WithCompressor(c compression.Compressor) CASOption {
	return func(cas *CAS) {
		cas.writer = c
	}
}

// WithRegistry replaces the default codec registry. Tests use this
// to feed a synthetic registry; production code uses the default
// (which has every codec we ship registered).
func WithRegistry(r *compression.CodecRegistry) CASOption {
	return func(cas *CAS) {
		cas.registry = r
	}
}

// WithEncryptor installs e as both the per-Put encryptor and the
// per-Get decryptor for chunks tagged with e.Algorithm() in their
// on-disk envelope. CAS chunks committed before e was installed (or
// committed by a CAS that had no encryptor) round-trip unchanged —
// their envelopes record EncryptionAlgo=AlgoNone.
//
// Passing the same Encryptor for both write and read is the v0.1
// default; per-tenant key rotation (multiple Encryptors registered
// for read, one for write) is a+ shape.
func WithEncryptor(e encryption.Encryptor) CASOption {
	return func(cas *CAS) {
		cas.encWriter = e
		if cas.encRegistry == nil {
			cas.encRegistry = encryption.NewRegistry()
		}
		cas.encRegistry.Register(e.Algorithm(), e)
	}
}

// WithEncryptionRegistry replaces the encryption registry. Tests use
// this to inject multiple decryptors (e.g. simulating key rotation).
// Production code calls WithEncryptor instead.
func WithEncryptionRegistry(r *encryption.CodecRegistry) CASOption {
	return func(cas *CAS) {
		cas.encRegistry = r
	}
}

// WithRetention configures WORM-style retention propagation. Every
// PutChunk includes RetainUntil + Mode in PutOptions. Storage
// backends with WORM support (S3 Object Lock, Azure immutable
// blob) honour the deadline as the deletion floor.
//
// Backends WITHOUT WORM support previously silently ignored the
// retention fields — an operator who configured WORM against a
// file:// or other non-WORM backend would believe data was
// protected when it wasn't.  After v23 audit, NewCAS detects
// this mismatch and PutChunk refuses with code
// `repo.cas.retention_unenforceable` until the operator either
// switches to a WORM-capable backend or explicitly opts out via
// WithRetentionAllowUnenforced.
//
// Pass a zero-valued CASRetention to disable (the default).
//
// Operators don't typically construct retention values directly —
// they pass them through from the repo metadata's WORM policy at
// CAS construction time via casdefault.NewWithRetention.
func WithRetention(r CASRetention) CASOption {
	return func(cas *CAS) {
		cas.retention = r
	}
}

// WithRetentionAllowUnenforced disables the retention-vs-backend
// safety check NewCAS otherwise enforces.  Use only in tests / dev
// environments where an operator knowingly points a WORM-config'd
// CAS at a backend without WORM support.  Production callers
// should never set this — the silent-acceptance footgun this
// exists to prevent (audit) is a compliance violation.
func WithRetentionAllowUnenforced() CASOption {
	return func(cas *CAS) {
		cas.retentionAllowUnenforced = true
	}
}

// WithChunkDurability sets the storage.Durability used for every
// PutChunk. The default (DurabilityInline) fsyncs each chunk before
// PutChunk returns. Pass DurabilityDeferred for a bulk writer that
// will call CAS.Barrier before committing — this turns ~1 fsync per
// chunk into ~1 fsync per Barrier, the core of the durability-modes
// throughput work. The caller is responsible for the Barrier: a
// deferred chunk is NOT crash-durable until Barrier returns nil.
func WithChunkDurability(d storage.Durability) CASOption {
	return func(cas *CAS) {
		cas.chunkDurability = d
	}
}

// WithSeenCacheLimit overrides the default bound on the in-memory
// positive (already-seen) cache. n <= 0 disables the bound entirely
// (the cache grows for the life of the CAS — only safe for a short-
// lived, single-operation CAS). A long-lived CAS (e.g. a `wal stream`
// session) should keep a finite limit so memory can't grow without
// bound. The limit is a soft ceiling: the cache is cleared wholesale
// when a fresh insert crosses it, never partially evicted, and dedup
// correctness is unaffected (the IfNotExists Put is the backstop).
func WithSeenCacheLimit(n int) CASOption {
	return func(cas *CAS) {
		if n < 0 {
			n = 0
		}
		cas.seenCap = n
	}
}

// WithDedupHints supplies a set of plaintext chunk hashes the caller
// believes are ALREADY present in the repo — typically every chunk
// referenced by the deployment's most recent prior backup manifest.
//
// When PutChunk is given a chunk whose hash is in this set and which it
// has not itself written this session, it issues one cheap Stat probe:
// a confirmed hit returns immediately, skipping compression, encryption
// and the upload; a miss — the hint was stale (chunk GC'd since the set
// was built) or a transient Stat error — falls through to the normal
// write path. The set is advisory only: correctness never depends on
// it, because the IfNotExists Put remains the backstop on every miss.
//
// A nil or empty set disables the probe entirely (zero behaviour
// change, zero overhead) — the right default for a first backup, which
// has no prior manifest to seed from.
//
// The CAS takes its OWN copy of the set, so PutChunk's lock-free reads of
// hints (it runs concurrently across the base-backup chunk worker pool)
// can't race a caller that keeps mutating the map it passed in after
// construction (data-race audit #5). The caller retains ownership of the
// argument and may do as it likes with it.
func WithDedupHints(h map[Hash]struct{}) CASOption {
	return func(cas *CAS) {
		if len(h) == 0 {
			return
		}
		hints := make(map[Hash]struct{}, len(h))
		for k := range h {
			hints[k] = struct{}{}
		}
		cas.hints = hints
	}
}

// defaultRegistry returns a fresh registry pre-populated with the
// none codec. zstd is registered by callers that import the zstd
// package (so a binary that doesn't need zstd doesn't drag it in).
//
// The CAS itself ALWAYS knows how to read AlgoNone — that's the
// minimum-viable round-trip path. Callers wanting zstd round-trip
// register it via WithRegistry.
func defaultRegistry() *compression.CodecRegistry {
	r := compression.NewRegistry()
	r.Register(compression.AlgoNone, none.Compressor{})
	return r
}

// NewCAS wraps sp. The caller retains ownership of sp and is responsible
// for Close.
func NewCAS(sp storage.StoragePlugin, opts ...CASOption) *CAS {
	if sp == nil {
		panic("repo: NewCAS requires a non-nil StoragePlugin")
	}
	c := &CAS{sp: sp, registry: defaultRegistry(), seenCap: defaultSeenCacheLimit}
	for _, opt := range opts {
		opt(c)
	}
	if c.writer == nil {
		c.writer = none.Compressor{}
	}
	if c.encRegistry == nil {
		c.encRegistry = encryption.NewRegistry()
	}
	// Auto-register the writer's algorithm into the read-back
	// registry. Without this, a CAS constructed via WithCompressor
	// with a codec the default registry doesn't know about would
	// silently fail every read of its own writes. We dedup against
	// existing registrations so callers that pre-register (the
	// casdefault path does this for backward-compat) don't trip the
	// "already registered" panic.
	if !c.registry.Has(c.writer.Algorithm()) {
		c.registry.Register(c.writer.Algorithm(), c.writer)
	}
	// WORM-vs-backend safety check.  When retention is configured
	// but the storage plugin lacks WORM, mark the CAS so PutChunk
	// refuses with a structured error.  Operators who knowingly
	// run this combination (dev / test repos) opt out via
	// WithRetentionAllowUnenforced. .
	if !c.retention.IsZero() && !c.retentionAllowUnenforced && !sp.Capabilities().WORM {
		c.retentionUnenforceable = true
	}
	return c
}

// ChunkInfo describes a chunk after a Put. Size reports the bytes that
// were hashed (matches the on-disk size for an unencrypted CAS; future
// envelope encryption will produce a different on-disk size, recorded
// separately by the encryption layer).
type ChunkInfo struct {
	Hash    Hash  `json:"hash"`
	Size    int64 `json:"size"`
	Deduped bool  `json:"deduped"`
}

// HexHash returns the lowercase-hex form of the chunk's hash.
//
// Deprecated: prefer Hash.String() directly. Kept for callers that
// already use this name.
func (c ChunkInfo) HexHash() string { return c.Hash.String() }

// PutChunk hashes body and stores it at its canonical key.
//
// If the chunk is already present (either in the in-memory positive cache
// or at the storage backend), the call is a no-op and ChunkInfo.Deduped
// is true. Concurrent Puts of the same content are race-safe: exactly
// one performs the actual write; the others observe ErrAlreadyExists at
// the storage layer and return Deduped=true.
//
// storage.ErrAlreadyExists is NEVER returned from PutChunk — that's an
// implementation detail of the IfNotExists semantics, swallowed here as
// the dedup-hit signal.
func (c *CAS) PutChunk(ctx context.Context, body []byte) (ChunkInfo, error) {
	if c.retentionUnenforceable {
		// Refuse rather than silently write chunks that the operator
		// believes are WORM-protected.  Compliance footgun the v23
		// audit flagged: file:// + WithRetention used to write a
		// "compliance" backup an admin could `rm -rf` ten seconds
		// later.
		return ChunkInfo{}, fmt.Errorf("%w (backend %q does not advertise WORM); switch to a WORM-capable backend (s3 with Object Lock, Azure immutable blob, NetApp SnapLock) or pass WithRetentionAllowUnenforced if you knowingly accept the gap",
			ErrRetentionUnenforceable, c.sp.Name())
	}
	hash := Hash(sha256.Sum256(body))
	// ChunkInfo.Size remains the PLAINTEXT length — that's what
	// manifests use for ChunkRef.Len, and what Restore concatenates.
	// The on-disk size after compression+envelope is recorded only
	// for diagnostic purposes.
	info := ChunkInfo{Hash: hash, Size: int64(len(body))}

	// Fast path: we've already seen this hash. Skip the storage roundtrip.
	if _, ok := c.seen.Load(hash); ok {
		c.dedupInMem.Add(1)
		info.Deduped = true
		return info, nil
	}

	// Hint path: the caller flagged this hash as probably already in
	// the repo. A Stat probe is far cheaper than compressing,
	// encrypting and uploading a chunk the repo already holds — and a
	// confirmed Stat lets us skip all of it. A stale hint (chunk GC'd
	// since the hint set was built, or a transient Stat error) just
	// falls through to the normal write path below; the IfNotExists
	// Put is the correctness backstop either way.
	if c.hints != nil {
		if _, hinted := c.hints[hash]; hinted {
			if _, statErr := c.sp.Stat(ctx, ChunkKey(hash)); statErr == nil {
				c.markSeen(hash)
				c.dedupStorage.Add(1)
				info.Deduped = true
				return info, nil
			}
		}
	}

	payload, algo, err := c.writer.Compress(body)
	if err != nil {
		return ChunkInfo{}, fmt.Errorf("cas: compress chunk %s: %w", info.HexHash(), err)
	}
	// Encryption layer — wraps the (possibly compressed) payload.
	// When encWriter is nil, encFields stays zero (EncryptionAlgo=0)
	// and the envelope's nonce field is all-zeros — the same shape
	// the unencrypted path produces.
	var encFields compression.EncryptionFields
	if c.encWriter != nil {
		ct, nonce, err := c.encWriter.Encrypt(payload)
		if err != nil {
			return ChunkInfo{}, fmt.Errorf("cas: encrypt chunk %s: %w", info.HexHash(), err)
		}
		payload = ct
		encFields.EncryptionAlgo = byte(c.encWriter.Algorithm())
		encFields.Nonce = nonce
	}
	envelope := compression.WriteEnvelope(algo, encFields, payload)

	key := ChunkKey(hash)
	putOpts := storage.PutOptions{
		IfNotExists:   true,
		ContentLength: int64(len(envelope)),
		Durability:    c.chunkDurability,
	}
	// ContentSHA256 here would be the hash of the ENVELOPE
	// bytes, not the plaintext — backends that consume it
	// verify the post-write integrity against this value.
	// Computing it costs ~9% of pg_hardstorage's wal-stream
	// CPU under the wal-stream profile (a SECOND full
	// SHA-256 pass over every chunk on top of the
	// content-addressing hash above).  Skip it when the
	// backend won't read it: S3 / Azure / GCS / SFTP / SCP
	// all rely on their own transport-layer integrity
	// (TLS, x-amz-content-sha256, SSH-channel MAC) and
	// silently discard PutOptions.ContentSHA256.  Only fs
	// returns VerifiesContentSHA256=true today.
	//
	// Plaintext-hash verification on the read side
	// (GetChunkBytes after envelope decode) is unaffected —
	// that's the chunk-content-addressing invariant, and
	// it always runs.
	if c.sp.Capabilities().VerifiesContentSHA256 {
		putOpts.ContentSHA256 = sha256.Sum256(envelope)
	}
	// WORM propagation: when retention is configured, include the
	// deadline + mode so WORM-capable backends apply the lock at
	// PUT time. Backends without WORM ignore these fields.
	if !c.retention.IsZero() {
		putOpts.RetainUntil = c.retention.RetainUntil
		putOpts.RetentionMode = c.retention.Mode
	}
	_, err = c.sp.Put(ctx, key, bytes.NewReader(envelope), putOpts)
	switch {
	case err == nil:
		c.markSeen(hash)
		c.dedupMiss.Add(1)
		return info, nil
	case errors.Is(err, storage.ErrAlreadyExists):
		c.markSeen(hash)
		c.dedupStorage.Add(1)
		info.Deduped = true
		return info, nil
	default:
		return ChunkInfo{}, fmt.Errorf("cas: put chunk %s: %w", info.HexHash(), err)
	}
}

// DedupStats returns a snapshot of this CAS's PutChunk dedup outcomes.
// Safe to call concurrently and at any time; the counts only grow, so a
// snapshot taken after the writer is done is exact. A base-backup
// runner reads it once the backup completes to report how much of the
// database was already in the repo.
func (c *CAS) DedupStats() DedupStats {
	return DedupStats{
		Misses:       c.dedupMiss.Load(),
		HitsInMemory: c.dedupInMem.Load(),
		HitsStorage:  c.dedupStorage.Load(),
	}
}

// Barrier makes every chunk written with DurabilityDeferred since
// the last Barrier crash-durable. It delegates to the storage
// plugin's Barrier. A caller that constructed the CAS with
// WithChunkDurability(DurabilityDeferred) MUST call Barrier — and
// see it return nil — before treating those chunks as committed
// (e.g. before writing a manifest that references them, or before
// reporting a flush LSN to PostgreSQL).
//
// On an InlineDurable backend (object stores) Barrier is a cheap
// no-op: every PutChunk was already durable on return.
func (c *CAS) Barrier(ctx context.Context) error {
	if err := c.sp.Barrier(ctx); err != nil {
		return fmt.Errorf("cas: barrier: %w", err)
	}
	return nil
}

// GetChunk returns a ReadCloser for the named chunk. Returns
// storage.ErrNotFound when absent. Caller closes.
//
// We do NOT verify the SHA-256 here on the way out (that would require
// reading the whole stream into memory). Callers that demand verified
// reads should use GetChunkBytes, which validates before returning.
func (c *CAS) GetChunk(ctx context.Context, hash Hash) (io.ReadCloser, error) {
	rc, err := c.sp.Get(ctx, ChunkKey(hash))
	if err != nil {
		return nil, fmt.Errorf("cas: get chunk %s: %w", hash, err)
	}
	return rc, nil
}

// GetChunkBytes fetches and returns the chunk's PLAINTEXT bytes.
// MaxChunkEnvelopeBytes caps how many bytes GetChunkBytes reads for a
// single on-disk chunk envelope before decoding. Chunks are bounded by
// the chunker (256 KiB plaintext default) and the decompressor caps the
// decompressed output at 256 MiB, so a larger envelope is corrupt or
// malicious; we refuse rather than slurp it unboundedly into memory
// (input-validation audit #3). 256 MiB is far above any legitimate chunk.
const MaxChunkEnvelopeBytes = 256 << 20

// readEnvelopeLimited reads up to max bytes from rc, erroring rather than
// allocating unboundedly when the source exceeds it.
func readEnvelopeLimited(rc io.Reader, max int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(rc, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > max {
		return nil, fmt.Errorf("cas: chunk envelope exceeds the %d-byte limit (refusing an oversized or malformed chunk)", max)
	}
	return body, nil
}

// The on-disk envelope is parsed, the codec recorded in the envelope
// is looked up in the CAS's registry, the payload is decompressed,
// and the resulting plaintext is SHA-256-verified against hash.
//
// Use this anywhere correctness matters — restoration, scrub,
// manifest verification.
func (c *CAS) GetChunkBytes(ctx context.Context, hash Hash) ([]byte, error) {
	rc, err := c.GetChunk(ctx, hash)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	envelope, err := readEnvelopeLimited(rc, MaxChunkEnvelopeBytes)
	if err != nil {
		return nil, fmt.Errorf("cas: read chunk %s: %w", hash, err)
	}
	algo, encFields, payload, err := compression.ReadEnvelope(envelope)
	if err != nil {
		return nil, fmt.Errorf("cas: decode envelope for chunk %s: %w", hash, err)
	}
	if encFields.IsEncrypted() {
		decryptor, lookupErr := c.encRegistry.Lookup(encryption.AlgorithmID(encFields.EncryptionAlgo))
		if lookupErr != nil {
			return nil, fmt.Errorf("cas: chunk %s: %w", hash, lookupErr)
		}
		pt, decErr := decryptor.Decrypt(payload, encFields.Nonce)
		if decErr != nil {
			return nil, fmt.Errorf("cas: decrypt chunk %s: %w", hash, decErr)
		}
		payload = pt
	}
	codec, err := c.registry.Lookup(algo)
	if err != nil {
		return nil, fmt.Errorf("cas: chunk %s: %w", hash, err)
	}
	body, err := codec.Decompress(payload)
	if err != nil {
		return nil, fmt.Errorf("cas: decompress chunk %s (algo=%s): %w", hash, algo, err)
	}
	got := Hash(sha256.Sum256(body))
	if got != hash {
		return nil, fmt.Errorf("cas: chunk %s: %w (stored bytes hash to %s)",
			hash, storage.ErrChecksumMismatch, got)
	}
	c.seen.Store(hash, struct{}{})
	return body, nil
}

// HasChunk reports whether the CAS contains the chunk. The in-memory
// positive cache short-circuits to true; otherwise we Stat.
func (c *CAS) HasChunk(ctx context.Context, hash Hash) (bool, error) {
	if _, ok := c.seen.Load(hash); ok {
		return true, nil
	}
	_, err := c.sp.Stat(ctx, ChunkKey(hash))
	switch {
	case err == nil:
		c.markSeen(hash)
		return true, nil
	case errors.Is(err, storage.ErrNotFound):
		return false, nil
	default:
		return false, fmt.Errorf("cas: stat chunk %s: %w", hash, err)
	}
}

// DeleteChunk removes a chunk by hash. Removing a non-existent chunk is
// a no-op (idempotent). The in-memory cache is updated to reflect the
// removal so a subsequent Has returns false.
//
// Direct deletion is dangerous outside of the GC subsystem: deleting a
// chunk that's still referenced by a manifest will break restores. The
// GC slice introduces the reference-counting that makes this safe.
func (c *CAS) DeleteChunk(ctx context.Context, hash Hash) error {
	if err := c.sp.Delete(ctx, ChunkKey(hash)); err != nil {
		return fmt.Errorf("cas: delete chunk %s: %w", hash, err)
	}
	c.unmarkSeen(hash)
	return nil
}

// markSeen records hash in the positive cache, keeping the cache bounded
// to seenCap entries. A fresh insert that would exceed the cap clears
// the cache wholesale — an O(1)-amortised bound that keeps memory finite
// on long-lived CAS instances (memory-leak audit #1). Clearing never
// affects correctness: a dropped entry costs at most one extra existence
// check on its next reference, behind the IfNotExists Put backstop.
func (c *CAS) markSeen(hash Hash) {
	if _, loaded := c.seen.LoadOrStore(hash, struct{}{}); loaded {
		return // already present — don't double-count
	}
	if c.seenCap <= 0 {
		return // bound disabled
	}
	if c.seenCount.Add(1) > int64(c.seenCap) {
		c.evictSeen()
	}
}

// unmarkSeen drops hash from the positive cache (DeleteChunk path),
// keeping seenCount in step.
func (c *CAS) unmarkSeen(hash Hash) {
	if _, ok := c.seen.LoadAndDelete(hash); ok && c.seenCap > 0 {
		c.seenCount.Add(-1)
	}
}

// evictSeen clears the positive cache once it has grown past seenCap.
// Guarded so that several markSeen calls crossing the threshold at once
// don't each pay the full clear; the recheck under the lock makes the
// loser a no-op. seenCount may drift by the number of concurrent inserts
// during a clear, so the cap is a soft ceiling — memory stays O(seenCap).
func (c *CAS) evictSeen() {
	c.seenMu.Lock()
	defer c.seenMu.Unlock()
	if c.seenCount.Load() <= int64(c.seenCap) {
		return // another goroutine already cleared
	}
	c.seen.Range(func(k, _ any) bool {
		c.seen.Delete(k)
		return true
	})
	c.seenCount.Store(0)
}

// ChunkKey is defined in chunkkey.go (production) and
// chunkkey_mutation_*.go (testkit-mutation variants under specific
// build tags).  Extracting it lets the testkit swap in
// deliberately-broken variants without touching this file.

// ParseChunkKey is the inverse of ChunkKey. It returns the parsed hash on
// success and ErrNotAChunkKey otherwise. Useful for the GC scanner.
func ParseChunkKey(key string) (Hash, error) {
	const prefix = "chunks/sha256/"
	const suffix = ".chk"
	var zero Hash
	if len(key) < len(prefix)+64+len(suffix) {
		return zero, ErrNotAChunkKey
	}
	if key[:len(prefix)] != prefix {
		return zero, ErrNotAChunkKey
	}
	if key[len(key)-len(suffix):] != suffix {
		return zero, ErrNotAChunkKey
	}
	// Within the middle: <aa>/<bb>/<aabb...60-more>
	rest := key[len(prefix) : len(key)-len(suffix)]
	if len(rest) != 2+1+2+1+64 || rest[2] != '/' || rest[5] != '/' {
		return zero, ErrNotAChunkKey
	}
	hexHash := rest[6:]
	if hexHash[:2] != rest[:2] || hexHash[2:4] != rest[3:5] {
		return zero, ErrNotAChunkKey
	}
	b, err := hex.DecodeString(hexHash)
	if err != nil {
		return zero, ErrNotAChunkKey
	}
	copy(zero[:], b)
	return zero, nil
}

// ErrNotAChunkKey indicates a key that doesn't match the chunk-key format.
var ErrNotAChunkKey = errors.New("repo: not a chunk key")

// ErrRetentionUnenforceable is returned by PutChunk when the CAS was
// configured with WithRetention but the underlying storage backend
// does not advertise WORM support.  Silent acceptance is a
// compliance-violating footgun (audit): an operator who
// believes their backups are deletion-protected would only discover
// the gap at audit time.  Operators who knowingly accept the gap
// (test / dev) opt out via WithRetentionAllowUnenforced.
var ErrRetentionUnenforceable = errors.New("repo: retention configured but storage backend lacks WORM support")
