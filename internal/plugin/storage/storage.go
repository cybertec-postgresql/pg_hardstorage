// Package storage defines the StoragePlugin contract that backs every
// pg_hardstorage repository.
//
// The same interface fronts the local filesystem, S3, Azure Blob, GCS, and
// any future tier-2 plugin shipped via hashicorp/go-plugin. The contract
// is intentionally narrow:
//
//   - Object-key addressed (no streaming append, no random access).
//   - All-or-nothing object semantics: a Put either fully succeeds or the
//     object is not visible to subsequent operations.
//   - Conditional writes (IfNotExists) and atomic rename (RenameIfNotExists)
//     are first-class — the resilience story depends on them.
//
// Plugins MUST implement Open, Put, Get, Stat, List, Delete, Close,
// RenameIfNotExists. SetRetention is optional (return ErrUnsupported when
// the backend lacks WORM); Capabilities advertises what's available.
package storage

import (
	"context"
	"errors"
	"io"
	"iter"
	"net/url"
	"time"
)

// Errors any backend may return. Use errors.Is to detect them — the fs and
// s3 plugins wrap their backend-specific errors with these sentinels so
// upper layers don't leak backend details.
var (
	ErrAlreadyExists    = errors.New("storage: object already exists")
	ErrNotFound         = errors.New("storage: object not found")
	ErrChecksumMismatch = errors.New("storage: checksum mismatch")
	ErrUnsupported      = errors.New("storage: capability not supported by backend")

	// ErrUnknownScheme is returned by Open when no plugin is registered
	// for the given URL scheme. The CLI maps it to a usage-error exit code.
	ErrUnknownScheme = errors.New("storage: no plugin registered for scheme")
)

// Capabilities advertises which optional features a plugin provides.
//
// Code that needs a capability MUST check Capabilities() first and fall
// back gracefully when absent. Operations that demand WORM (legal hold,
// regulatory backups) refuse to proceed when WORM is false.
type Capabilities struct {
	WORM                   bool `json:"worm"`
	ConditionalPut         bool `json:"conditional_put"`
	Multipart              bool `json:"multipart"`
	ServerSideEncryption   bool `json:"server_side_encryption"`
	CrossRegionReplicate   bool `json:"cross_region_replicate"`
	StorageClassSelectable bool `json:"storage_class_selectable"`

	// VerifiesContentSHA256, when true, signals that this
	// plugin's Put implementation actually consumes
	// PutOptions.ContentSHA256 — either by verifying the
	// post-write hash against it or by passing it to a
	// network protocol that does (e.g. S3's
	// x-amz-checksum-sha256 header).  The CAS layer uses
	// this to skip computing the envelope hash when no
	// backend will read it; the wal-stream profile showed
	// that hash burning ~9% of CPU on backends that
	// silently discarded it.
	//
	// Today only the fs plugin returns true — it computes
	// SHA-256 inline during the write and verifies against
	// the caller's value.  S3, Azure, GCS, SFTP, SCP all
	// rely on their own transport-layer integrity checks
	// (TLS, S3 SDK's Content-MD5 / x-amz-content-sha256,
	// SFTP's SSH-channel MAC) and ignore the field, so
	// they return false.
	VerifiesContentSHA256 bool `json:"verifies_content_sha256"`

	// InlineDurable is true when a successful Put is durable the
	// moment it returns, regardless of PutOptions.Durability — the
	// backend has no writeback cache the caller must fsync past.
	// Object stores (S3, Azure, GCS) set this: a 200-OK PUT is
	// server-side durable. The fs backend sets it false — a Put's
	// bytes sit in the OS page cache until fsync.
	InlineDurable bool `json:"inline_durable"`

	// DurabilityBarrier is true when the plugin's Barrier actually
	// makes prior DurabilityDeferred writes durable. The fs backend
	// sets it true (Barrier fsyncs the deferred files + dirs). A
	// backend that has BOTH InlineDurable and DurabilityBarrier
	// false has no way to make a deferred write durable — callers
	// that need durability MUST use DurabilityInline there.
	DurabilityBarrier bool `json:"durability_barrier"`
}

// Durability controls when a Put's bytes become crash-durable.
//
// The zero value, DurabilityInline, is the safe default: the Put does
// not return until the object is durable (fsync'd on fs; PUT-ack'd on
// an object store). DurabilityDeferred lets a caller batch many writes
// and pay a single fsync barrier for all of them — the caller MUST
// then call Barrier before treating any deferred write as committed
// (e.g. before writing a manifest that references those chunks, or
// before reporting a flush LSN to PostgreSQL).
type Durability int

const (
	// DurabilityInline makes the Put durable before it returns.
	DurabilityInline Durability = iota
	// DurabilityDeferred skips the per-Put fsync; the write is
	// durable only after a subsequent successful Barrier.
	//
	// On a backend that is not InlineDurable (the fs plugin) a
	// DurabilityDeferred object MAY NOT be visible at its key — to
	// Get, Stat or List — until Barrier returns: the bytes are
	// staged out of the way and only published to the key once they
	// are crash-durable, so a crash before Barrier can never leave a
	// truncated object at a real key. Callers MUST therefore Barrier
	// before reading a deferred write back or committing a manifest
	// that references it. InlineDurable backends ignore Durability
	// entirely — every Put is immediately visible and durable.
	DurabilityDeferred
)

// NopBarrier is an embeddable no-op Barrier for StoragePlugin
// implementations that never have deferred writes to flush — object
// stores (a PUT is durable on return) and test fakes. Embed it to
// satisfy the interface without a hand-written method:
//
//	type myPlugin struct { storage.NopBarrier; /* ... */ }
type NopBarrier struct{}

// Barrier is a no-op — there is nothing buffered to make durable.
func (NopBarrier) Barrier(context.Context) error { return nil }

// WORMMode mirrors S3 Object Lock semantics. Compliance is the regulatory-
// grade mode (no early deletion); Governance allows authorized override.
type WORMMode string

const (
	// WORMNone disables Object Lock; the object can be overwritten or
	// deleted on the normal path.
	WORMNone WORMMode = ""
	// WORMGovernance applies retention that authorised principals can
	// shorten or remove (the s3:BypassGovernanceRetention permission
	// on AWS, equivalent ACLs elsewhere).
	WORMGovernance WORMMode = "governance"
	// WORMCompliance applies retention that nobody — including the
	// root account — can shorten until the period expires. The
	// regulatory-grade mode.
	WORMCompliance WORMMode = "compliance"
)

// PutOptions controls a single Put. Zero-value is "best effort overwrite,
// no conditions, no retention".
type PutOptions struct {
	// IfNotExists makes the Put atomically conditional. Returns
	// ErrAlreadyExists if the key is present. Required for chunk dedup
	// and manifest commit to be safe under concurrency.
	IfNotExists bool

	// ContentLength is the expected number of bytes. Optional but lets
	// backends pre-allocate / pick optimal multipart strategy.
	ContentLength int64

	// ContentSHA256 is the expected SHA-256 of the plaintext. When set
	// (any non-zero byte), the plugin MUST verify after writing and
	// return ErrChecksumMismatch on disagreement. Zero value disables
	// the check.
	ContentSHA256 [32]byte

	// StorageClass is backend-specific (S3: STANDARD / GLACIER / ...).
	// Empty string means "use the backend default".
	StorageClass string

	// RetainUntil sets a per-object retention time when WORM is enabled.
	// Ignored when the backend does not support WORM.
	RetainUntil time.Time

	// RetentionMode selects the WORM lock posture (Compliance or
	// Governance) when RetainUntil is set. Empty implies Compliance
	// — the regulatory-grade default. Backends without WORM ignore
	// this field.
	RetentionMode WORMMode

	// Metadata is a small string -> string map serialized as object
	// metadata where the backend supports it. Keys must be ASCII.
	Metadata map[string]string

	// Durability selects whether this Put is durable before it
	// returns (DurabilityInline, the zero-value default) or only
	// after a later Barrier (DurabilityDeferred). Backends that are
	// InlineDurable ignore the field — every Put is durable anyway.
	Durability Durability
}

// PutResult reports what the backend committed. Backends that don't have
// a useful ETag may set it to the lowercase-hex SHA-256.
type PutResult struct {
	Key           string
	Size          int64
	ContentSHA256 [32]byte
	ETag          string
	VersionID     string
}

// ObjectInfo describes an object found via Stat or List. The Size, ModTime
// and ContentSHA256 are best-effort: not every backend exposes a strong
// content hash, and ContentSHA256 may be the zero value when unknown.
type ObjectInfo struct {
	Key           string            `json:"key"`
	Size          int64             `json:"size"`
	ModTime       time.Time         `json:"mod_time"`
	ContentSHA256 [32]byte          `json:"-"`
	ETag          string            `json:"etag,omitempty"`
	StorageClass  string            `json:"storage_class,omitempty"`
	VersionID     string            `json:"version_id,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
}

// StorageConfig is what a plugin's Open consumes. The URL is the canonical
// address; per-backend extras (region, credentials, bucket name) are
// pulled from URL host/path/query and from Extras.
type StorageConfig struct {
	URL    *url.URL
	Extras map[string]string
}

// StoragePlugin is the contract every backend implements.
//
// All methods are goroutine-safe unless an implementation documents
// otherwise. Concurrent Puts to the same key are resolved by IfNotExists
// (only one wins) or by the backend's last-writer-wins semantics when
// IfNotExists is false.
type StoragePlugin interface {
	// Name is the canonical lowercase backend name ("fs", "s3", ...).
	Name() string

	// Open initializes the plugin against cfg. Idempotent.
	Open(ctx context.Context, cfg StorageConfig) error

	// Put writes the contents of r at key. Returns ErrAlreadyExists when
	// IfNotExists was true and the key was already present. Returns
	// ErrChecksumMismatch when the verified hash disagrees with
	// PutOptions.ContentSHA256.
	Put(ctx context.Context, key string, r io.Reader, opts PutOptions) (PutResult, error)

	// Get returns a ReadCloser for key. Returns ErrNotFound when the key
	// is absent. Caller closes.
	Get(ctx context.Context, key string) (io.ReadCloser, error)

	// Stat returns ObjectInfo for key. Returns ErrNotFound when absent.
	Stat(ctx context.Context, key string) (ObjectInfo, error)

	// List streams objects whose key begins with prefix. The iterator
	// yields (info, nil) for each object and (zero, err) on a fatal
	// listing error; consumers stop on either.
	List(ctx context.Context, prefix string) iter.Seq2[ObjectInfo, error]

	// Delete removes key. Removing a non-existent key is a no-op (no
	// ErrNotFound), so retried deletes are safe.
	Delete(ctx context.Context, key string) error

	// RenameIfNotExists atomically renames src -> dst, failing with
	// ErrAlreadyExists when dst is present. Used to commit manifests
	// from <name>.tmp -> <name>. Backends that can't atomically check-
	// and-link MUST emulate the semantics correctly even at higher cost.
	RenameIfNotExists(ctx context.Context, src, dst string) error

	// SetRetention applies a retention deadline + WORM mode to key.
	// Returns ErrUnsupported when the backend lacks WORM.
	SetRetention(ctx context.Context, key string, until time.Time, mode WORMMode) error

	// Barrier makes every preceding DurabilityDeferred Put durable.
	// After Barrier returns nil, those writes are guaranteed to
	// survive a crash, exactly as if each had used DurabilityInline.
	//
	// Backends whose Put is already durable on return (object
	// stores) implement Barrier as a no-op — see NopBarrier.
	// Calling Barrier with no deferred writes outstanding is always
	// safe and cheap. Barrier is goroutine-safe with concurrent Puts:
	// it flushes everything deferred up to the moment it was called.
	Barrier(ctx context.Context) error

	// Capabilities advertises optional features.
	Capabilities() Capabilities

	// Close releases any backend connections / handles.
	Close() error
}

// RegionAware is an OPTIONAL interface a StoragePlugin can implement
// to expose its operating region for compliance / data-residency
// gating. Plugins that don't implement it (e.g. the fs plugin, where
// "region" isn't a meaningful concept) fall back to RegionUnknown
// via the RegionOf helper below.
//
// Returned values are operator-facing strings — for s3 they're
// AWS region codes ("us-east-1", "eu-west-1", "ap-northeast-1"). The
// residency-check matcher does case-insensitive prefix and
// suffix-pair matching ("eu" matches "eu-west-1") so operators can
// declare residency at the granularity that matches their compliance
// requirement.
type RegionAware interface {
	Region() string
}

// FreeSpaceInfo describes a backend's available capacity at the
// repo root. Returned by storage plugins that implement the
// optional FreeSpaceAware interface (today: fs); unsupported on
// object stores where "free space" isn't a meaningful concept
// (the operator's quota is set out-of-band; we can't probe it).
type FreeSpaceInfo struct {
	// TotalBytes is the size of the underlying volume / storage
	// pool the repo lives on. Useful for "how much headroom
	// would I have on a full disk?" reasoning. Zero when
	// Unsupported.
	TotalBytes int64

	// AvailableBytes is what an unprivileged process can write
	// before encountering ENOSPC. On Unix this is statfs.Bavail
	// × statfs.Bsize — accounts for reserved-blocks-for-root.
	// Zero when Unsupported.
	AvailableBytes int64

	// Unsupported is true when the backend can't report
	// capacity (object stores, custom plugins). Capacity-
	// preflight code branches on this rather than treating
	// 0 free as "the disk is full."
	Unsupported bool
}

// FreeSpaceAware is an OPTIONAL interface a StoragePlugin can
// implement to expose its current available capacity. Plugins
// that don't implement it report Unsupported via the
// FreeSpaceOf helper. Capacity pre-flight (refuse a backup
// whose projected size exceeds the repo's free space)
// consults this through the helper, so unsupported backends
// silently pass the gate — same posture as RegionAware /
// RegionOf.
type FreeSpaceAware interface {
	FreeSpace(ctx context.Context) (FreeSpaceInfo, error)
}

// FreeSpaceOf calls sp.FreeSpace if sp implements
// FreeSpaceAware, else returns FreeSpaceInfo{Unsupported: true}
// and a nil error. Callers branch on Unsupported to decide
// whether the pre-flight applies.
//
// Errors from a FreeSpaceAware plugin propagate verbatim —
// the caller decides whether to fail-closed (refuse the
// operation) or fail-open (skip the pre-flight and let the
// backend ENOSPC mid-write). Today's pre-flight fails-open
// because a flaky statfs shouldn't refuse an otherwise-OK
// backup.
func FreeSpaceOf(ctx context.Context, sp StoragePlugin) (FreeSpaceInfo, error) {
	if fs, ok := sp.(FreeSpaceAware); ok {
		return fs.FreeSpace(ctx)
	}
	return FreeSpaceInfo{Unsupported: true}, nil
}

// RegionUnknown is the canonical "no meaningful region" value. The
// fs plugin and any future local-only backend report this. The
// residency check refuses on RegionUnknown when the deployment has
// any residency restriction declared — explicit > clever.
const RegionUnknown = ""

// RegionOf returns sp.Region() if sp implements RegionAware, else
// RegionUnknown. Callers (the backup orchestrator's pre-flight, the
// `residency check` command, doctor) use this rather than the
// type-assertion directly so the optional-interface pattern is
// concentrated in one place.
func RegionOf(sp StoragePlugin) string {
	if r, ok := sp.(RegionAware); ok {
		return r.Region()
	}
	return RegionUnknown
}
