// Package casdefault wires the standard set of compression codecs
// into a freshly-constructed CAS. Importing this package's New
// function gives you a CAS that:
//
//   - writes new chunks using zstd (level: SpeedBetterCompression)
//   - reads chunks compressed with any registered algorithm (none, zstd)
//
// We keep the wiring out of the bare repo package so a tiny binary
// (e.g. a future inspection-only tool that never needs to write or
// decode zstd) can construct a no-codec CAS without dragging the
// zstd implementation in.
package casdefault

import (
	"time"

	klauspost "github.com/klauspost/compress/zstd"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/compression/zstd"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// compressorFor maps the operator-facing compression preset
// to the klauspost zstd encoder level.  Single source of
// truth for the level translation; every casdefault entry
// point goes through this.
//
// Profiling under a 10-GB-WAL workload:
//
//	balanced  (SpeedBetterCompression)  ~40% of pg_hs CPU
//	fast      (SpeedDefault)            ~20% of pg_hs CPU,  +10-15% on-disk size
//	max       (SpeedBestCompression)    ~80-100% of pg_hs CPU,  ~5% smaller
//
// The default stays balanced for back-compat with v0.1..
// repos that have no Compression field in HSREPO; their
// Resolved() lands here as balanced too.
func compressorFor(level repo.CompressionLevel) *zstd.Compressor {
	switch level.Resolved() {
	case repo.CompressionFast:
		return zstd.New(klauspost.SpeedDefault)
	case repo.CompressionMax:
		return zstd.New(klauspost.SpeedBestCompression)
	default:
		// CompressionBalanced — same level NewDefault has
		// always returned (SpeedBetterCompression).
		return zstd.NewDefault()
	}
}

// Option tunes a casdefault constructor.  Today the only
// option is WithCompressionLevel; future tuning (writer
// thread count, codec switch) plugs in through the same
// variadic surface so existing call sites stay
// API-compatible.
type Option func(*config)

type config struct {
	level      repo.CompressionLevel
	durability storage.Durability
	hints      map[repo.Hash]struct{}
}

// WithCompressionLevel pins the zstd encoder level for new
// chunks.  Empty / unset → CompressionBalanced for
// back-compat with v0.1..repos that have no
// Compression field in HSREPO.  Writer paths (wal-stream,
// backup runner) read meta.Compression from the repo's
// HSREPO and pass it through.  Read-only callers
// (restore, scrub, repair, gc, verify) don't care — the
// decoder handles every level — and keep using the bare
// constructors.
func WithCompressionLevel(l repo.CompressionLevel) Option {
	return func(c *config) { c.level = l }
}

// WithChunkDurability sets the storage.Durability used for every
// PutChunk. The default (DurabilityInline) fsyncs each chunk before
// PutChunk returns. A bulk writer — the base-backup runner, the WAL
// streamer — passes DurabilityDeferred and then calls CAS.Barrier
// at its commit boundary, turning ~1 fsync per chunk into ~1 fsync
// per Barrier. Read-only callers leave it at the default.
func WithChunkDurability(d storage.Durability) Option {
	return func(c *config) { c.durability = d }
}

// WithDedupHints passes a set of chunk hashes the caller believes are
// already in the repo down to the CAS — see repo.WithDedupHints. The
// base-backup runner seeds this from the deployment's most recent
// prior manifest so a re-backup confirms each unchanged chunk with a
// single Stat probe instead of re-compressing and re-uploading it.
// nil/empty is a no-op (a first backup has no prior manifest).
func WithDedupHints(h map[repo.Hash]struct{}) Option {
	return func(c *config) { c.hints = h }
}

func resolveOpts(opts ...Option) config {
	var c config
	for _, o := range opts {
		o(&c)
	}
	return c
}

// New returns a CAS with zstd as the default writer. NewCAS auto-
// registers the writer's algorithm into the read-back registry, so
// every chunk written here round-trips without further wiring. The
// AlgoNone codec is registered by NewCAS's defaultRegistry, so any
// chunks committed unencrypted/uncompressed (legacy or test) also
// read back correctly.
//
// Variadic Options pin tunables (compression level today;
// future extensibility lives here too).  Pre-Option call
// sites remain valid: New(sp) → balanced-level zstd, the
// v0.1..default.
func New(sp storage.StoragePlugin, opts ...Option) *repo.CAS {
	c := resolveOpts(opts...)
	return repo.NewCAS(sp,
		repo.WithCompressor(compressorFor(c.level)),
		repo.WithChunkDurability(c.durability),
		repo.WithDedupHints(c.hints),
	)
}

// NewEncrypted is New + an Encryptor wired in. Every PutChunk
// compresses (zstd by default) and then encrypts; every GetChunkBytes
// decrypts (using the same encryptor) and then decompresses.
//
// The Encryptor's key is the per-backup DEK — generated by the
// runner and wrapped under the operator's KEK before going into the
// manifest. casdefault doesn't see the KEK; that custody chain is
// the runner's job.
func NewEncrypted(sp storage.StoragePlugin, enc encryption.Encryptor, opts ...Option) *repo.CAS {
	c := resolveOpts(opts...)
	return repo.NewCAS(sp,
		repo.WithCompressor(compressorFor(c.level)),
		repo.WithChunkDurability(c.durability),
		repo.WithDedupHints(c.hints),
		repo.WithEncryptor(enc),
	)
}

// NewWithRetention is New + a WORM retention policy wired in.
// Every PutChunk's PutOptions carry RetainUntil + RetentionMode so
// WORM-capable backends (S3 Object Lock, Azure immutable blob)
// apply the lock at write time. Backends without WORM (fs)
// silently ignore the field.
//
// `now` is captured at construction so all chunks committed
// through this CAS get the SAME retention deadline (rather than
// drifting per-chunk by a few seconds). Operators wanting a
// per-PUT deadline construct multiple CAS instances or extend
// repo.CAS to accept a deadline-per-Put callback (not v1 scope).
//
// When policy is nil or zero, this returns a regular New(sp) CAS
// with no retention propagation — the caller's policy plumbing
// can pass nil safely.
func NewWithRetention(sp storage.StoragePlugin, policy *repo.WORMPolicy, now time.Time, opts ...Option) *repo.CAS {
	if policy.IsZero() {
		return New(sp, opts...)
	}
	c := resolveOpts(opts...)
	return repo.NewCAS(sp,
		repo.WithCompressor(compressorFor(c.level)),
		repo.WithChunkDurability(c.durability),
		repo.WithDedupHints(c.hints),
		repo.WithRetention(repo.CASRetention{
			RetainUntil: policy.RetainUntil(now),
			Mode:        storage.WORMMode(policy.Mode),
		}),
	)
}

// NewEncryptedWithRetention is the encrypted + retention combo.
func NewEncryptedWithRetention(sp storage.StoragePlugin, enc encryption.Encryptor, policy *repo.WORMPolicy, now time.Time, opts ...Option) *repo.CAS {
	if policy.IsZero() {
		return NewEncrypted(sp, enc, opts...)
	}
	c := resolveOpts(opts...)
	return repo.NewCAS(sp,
		repo.WithCompressor(compressorFor(c.level)),
		repo.WithChunkDurability(c.durability),
		repo.WithDedupHints(c.hints),
		repo.WithEncryptor(enc),
		repo.WithRetention(repo.CASRetention{
			RetainUntil: policy.RetainUntil(now),
			Mode:        storage.WORMMode(policy.Mode),
		}),
	)
}
