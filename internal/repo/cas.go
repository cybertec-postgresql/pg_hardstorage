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
	// PutChunk's PutOptions.RetainUntil. Zero disables (unless
	// RetainUntilFunc is set).
	RetainUntil time.Time
	// RetainUntilFunc, when non-nil, is evaluated at EACH PutChunk to
	// recompute the deadline, and takes precedence over RetainUntil.
	// Long-running writers need this: a deadline fixed at construction
	// under-locks every chunk written after construction, so a WAL stream
	// running longer than the retention window would leave its older WAL
	// deletable before term while the per-segment manifest (locked with the
	// current time) stays immutable — a WORM repo whose data outlives its
	// lock. Bounded writers (backup, wal push) leave it nil and use the
	// fixed RetainUntil.
	RetainUntilFunc func() time.Time
	// Mode is the WORMMode propagated alongside the deadline.
	// Backends use this to choose the lock posture (compliance
	// vs governance). Empty disables, regardless of RetainUntil.
	Mode storage.WORMMode
}

// IsZero reports whether retention is unconfigured.
func (r CASRetention) IsZero() bool {
	if r.Mode == storage.WORMNone {
		return true
	}
	return r.RetainUntil.IsZero() && r.RetainUntilFunc == nil
}

// retainUntil resolves the deadline for one PutChunk: the per-call clock
// when configured (long-running writers), else the fixed deadline.
func (r CASRetention) retainUntil() time.Time {
	if r.RetainUntilFunc != nil {
		return r.RetainUntilFunc()
	}
	return r.RetainUntil
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

	// adopted records every hash this CAS instance DEDUPLICATED
	// AGAINST rather than wrote: a hint-confirmed Stat hit, or an
	// IfNotExists put lost to a concurrent writer. These are the
	// chunks whose durability this backup is TRUSTING without having
	// written them — and therefore the only chunks a concurrent
	// `repo gc --apply` can pull out from under a committing backup.
	// gc's --min-chunk-age floor protects everything this run wrote
	// (young mtime); an adopted chunk is old by definition — an orphan
	// from an expired tombstone whose content reappears today gets a
	// dedup hit that touches no object and refreshes no mtime.
	// AdoptedHashes exposes the set so the backup runner can re-verify
	// existence at manifest-commit time, closing the sweep race from
	// the writer's side no matter how gc's own guards are timed.
	adopted sync.Map // Hash -> struct{}

	// adopt guards the one-shot cross-DEK check. See ensureAdoptable.
	adopt adoptGuard
}

// adoptGuard tracks the one-shot verification that chunks ALREADY in
// the repository are readable with THIS backup's DEK.
//
// Dedup adopts an existing chunk by plaintext hash. Chunk keys are
// global to the repository (chunks/sha256/<hash>.chk), but the shared
// DEK is per-KEKRef — sharedkey.ResolveOrMint is called with one
// kekRef and mints for that ref alone. So a backup under KEKRef B that
// dedups against chunks written under KEKRef A produces a manifest
// referencing chunks only DEK-A can decrypt: the backup exits 0 and
// only fails at restore.
//
// That is the failure sharedkey.go's own header describes for issue
// #31, reached through a different door — the existing guard
// serialises minting WITHIN one KEKRef and nothing considered a
// second one.
//
// Detection rather than prevention is deliberate. Per-tenant KEKs
// sharing a repository is a documented, compliance-load-bearing
// configuration (docs/compliance/hipaa.md §164.312(a)(1);
// rotate-kek.md's multi-tenant section), so refusing every multi-KEK
// repository would break a supported setup. Tenants whose data does
// not overlap never produce a dedup hit against each other and are
// unaffected; only an ACTUAL collision fails.
//
// One probe suffices: the check is a property of (repo, DEK), not of
// any single chunk, so the first adoption answers it for the whole
// backup. Verifying every hit would read and decrypt each deduplicated
// chunk, which is precisely the work dedup exists to avoid.
type adoptGuard struct {
	mu       sync.Mutex
	resolved bool
	err      error

	// multiKEK records whether this repository holds shared-DEK
	// objects for MORE THAN ONE KEKRef, and whether we have looked.
	//
	// The one-probe shortcut above rests on "the check is a property
	// of (repo, DEK)". That is true of a repository whose chunks were
	// all written under one KEK, and FALSE of a mixed one — which is
	// the configuration this guard exists for. In a mixed repo the
	// answer depends on WHICH chunk is adopted, so a first adoption
	// that happens to hit a readable chunk resolved the guard OK and
	// every later foreign-DEK adoption sailed through unchecked. The
	// backup then committed a manifest referencing chunks it cannot
	// decrypt: exit 0, failure at restore — precisely the outcome the
	// guard was written to prevent, reached by adopting in a different
	// order.
	//
	// So verify the premise instead of assuming it. One LIST of the
	// shared-DEK prefix says whether the repo is single-KEK; if it is,
	// nothing changes and dedup keeps its cost profile. If it is not,
	// every adoption is checked, because in a mixed repo that is the
	// only answer worth having.
	multiKEK      bool
	multiKEKKnown bool
}

// sharedDEKPrefix mirrors sharedkey.sharedDEKPrefix — one object per
// KEKRef that has minted a shared DEK in this repository. Duplicated
// rather than imported because repo cannot depend on repo/sharedkey
// (sharedkey imports storage and is layered above the CAS); the
// constant is part of the on-disk layout and changes with it.
const sharedDEKPrefix = "keys/shared-dek/"

// repoIsMultiKEK reports whether the repository holds shared-DEK
// objects for more than one KEKRef.
//
// Conservative on failure: a LIST error returns false, keeping the
// cheap path rather than failing a backup over a listing blip. That
// weakens detection in a repo we could not read, which is strictly
// better than today's behaviour and never worse.
func (c *CAS) repoIsMultiKEK(ctx context.Context) bool {
	n := 0
	for _, err := range c.sp.List(ctx, sharedDEKPrefix) {
		if err != nil {
			return false
		}
		n++
		if n > 1 {
			return true
		}
	}
	return false
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

// AdoptedHashes returns every hash this CAS instance deduplicated
// against without writing (hint-confirmed Stat hits and lost
// IfNotExists races). The backup runner re-Stats these at
// manifest-commit time: they are the only chunks a concurrent
// `repo gc --apply` can delete out from under a backup that then
// commits a manifest referencing them, because gc's --min-chunk-age
// floor covers only chunks with a young mtime — i.e. the ones this
// run wrote. Order is unspecified.
// WasAdopted reports whether this CAS instance deduplicated against
// hash without writing it. Commit paths use it to gate exactly the
// chunks whose durability was trusted rather than produced — see
// AdoptedHashes.
func (c *CAS) WasAdopted(h Hash) bool {
	_, ok := c.adopted.Load(h)
	return ok
}

func (c *CAS) AdoptedHashes() []Hash {
	var out []Hash
	c.adopted.Range(func(k, _ any) bool {
		out = append(out, k.(Hash))
		return true
	})
	return out
}

// ForgetAdopted drops the given hashes from this CAS's adoption set.
//
// Callers MUST only forget a hash AFTER a manifest that references it
// has committed (or been verified as an already-committed idempotent
// re-commit). Once a committed manifest references a chunk, gc's
// orphan sweep can no longer delete it — the manifest is a referent —
// so the commit-time re-verification that the adoption set exists for
// (AdoptedHashes / WasAdopted) can never need the entry again, even
// if a later segment or backup re-adopts the same hash: the earlier
// committed manifest already implies the chunk's continued presence.
//
// This is what keeps a long-lived CAS bounded: `wal stream` reuses one
// CAS for days or weeks, and without the release the set would retain
// every deduplicated hash ever seen (memory-leak audit #2).
//
// Forgetting a hash that was never adopted is a no-op; calling it
// twice is harmless.
func (c *CAS) ForgetAdopted(hashes ...Hash) {
	for _, h := range hashes {
		c.adopted.Delete(h)
	}
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
				if err := c.ensureAdoptable(ctx, hash); err != nil {
					return ChunkInfo{}, err
				}
				c.markSeen(hash)
				c.markAdopted(hash)
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
		putOpts.RetainUntil = c.retention.retainUntil()
		putOpts.RetentionMode = c.retention.Mode
	}
	_, err = c.sp.Put(ctx, key, bytes.NewReader(envelope), putOpts)
	switch {
	case err == nil:
		c.markSeen(hash)
		c.dedupMiss.Add(1)
		return info, nil
	case errors.Is(err, storage.ErrAlreadyExists):
		if aerr := c.ensureAdoptable(ctx, hash); aerr != nil {
			return ChunkInfo{}, aerr
		}
		c.markSeen(hash)
		c.markAdopted(hash)
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

// ensureAdoptable verifies, once per CAS, that a chunk already present
// in the repository can be read with this backup's DEK before dedup
// adopts it.
//
// Returns nil when there is no DEK in play (an unencrypted repo cannot
// have this problem) or when the probe succeeds. A TRANSIENT read
// failure is not conclusive and leaves the guard unresolved so a later
// adoption retries; only a decrypt or content-hash failure — which
// means the stored bytes are not ours — is recorded and fails the
// backup.
func (c *CAS) ensureAdoptable(ctx context.Context, hash Hash) error {
	if c.encWriter == nil {
		return nil
	}
	c.adopt.mu.Lock()
	defer c.adopt.mu.Unlock()

	// A recorded REFUSAL is always conclusive and always cached: the
	// repository holds chunks this DEK cannot read, and that does not
	// stop being true.
	if c.adopt.resolved && c.adopt.err != nil {
		return c.adopt.err
	}
	if !c.adopt.multiKEKKnown {
		c.adopt.multiKEK = c.repoIsMultiKEK(ctx)
		c.adopt.multiKEKKnown = true
	}
	// A recorded SUCCESS generalises only in a single-KEK repository.
	if c.adopt.resolved && !c.adopt.multiKEK {
		return nil
	}

	_, err := c.GetChunkBytes(ctx, hash)
	if err == nil {
		c.adopt.resolved = true
		return nil
	}
	if !errors.Is(err, encryption.ErrAuthenticationFailed) &&
		!errors.Is(err, storage.ErrChecksumMismatch) {
		// Network blip, throttling, a transient backend error: not
		// evidence about key custody. Leave unresolved.
		return nil
	}

	c.adopt.resolved = true
	c.adopt.err = fmt.Errorf(
		"cas: chunk %s is already in this repository but does not decrypt with this "+
			"backup's data key (%w). Deduplicating against it would commit a manifest "+
			"referencing chunks it cannot read — a backup that succeeds and then fails "+
			"at restore. This happens when a repository holds chunks written under a "+
			"DIFFERENT kek_ref: chunk keys are global to the repository, but the shared "+
			"DEK is per-KEKRef. Either give this deployment its own repository, or run "+
			"`pg_hardstorage kms rotate` so every manifest shares one KEK (rotation "+
			"re-wraps the SAME DEK, so existing chunks stay readable)",
		hash, err)
	return c.adopt.err
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
		// Deliberately NOT markSeen. Every entry in the positive cache
		// must mean "present AND readable by THIS CAS's DEK", because
		// PutChunk's fast path returns Deduped straight out of it
		// without consulting ensureAdoptable. Every other writer
		// upholds that: the hint path and the ErrAlreadyExists path
		// check adoptability first, a successful Put is our own bytes,
		// and GetChunkBytes caches only after a verified read.
		//
		// A bare Stat proves presence, not readability. Caching on it
		// would let a caller that asks "is this chunk here?" before
		// writing prime the cache with a chunk written under a
		// DIFFERENT KEKRef's DEK; the next PutChunk would dedup against
		// it and commit a manifest referencing chunks it cannot
		// decrypt — the backup exits 0 and fails at restore, which is
		// precisely what the adopt guard exists to prevent, reached by
		// another door. Nothing calls HasChunk today, so the door was
		// shut by accident; this keeps it shut by construction.
		//
		// The cost of not caching is one extra Stat on a later
		// reference, behind the IfNotExists Put backstop.
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
