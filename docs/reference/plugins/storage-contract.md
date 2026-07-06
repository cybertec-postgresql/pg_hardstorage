---
title: Storage plugin contract
description: The StoragePlugin interface — every method, every error, every capability flag.
tags:
  - plugins
  - storage
  - reference
---

# Storage plugin contract

A storage plugin fronts an object store: filesystem, S3,
Azure Blob, GCS, SFTP, or a Tier-2 binary you ship.  The
contract is intentionally narrow — object-key addressed,
all-or-nothing object semantics, and conditional writes
(`IfNotExists`) and atomic rename
(`RenameIfNotExists`) as first-class operations.  The
correctness of pg_hardstorage's CAS, manifest commit, and
WORM enforcement depends on the IfNotExists / Rename
contracts holding exactly as documented.

!!! note "Reference implementations"
    - `internal/plugin/storage/fs/fs.go` — filesystem,
      including `RenameIfNotExists` via `linkat(2) | rename(2)`
      and the per-OS `statfs` shims for `FreeSpace`.
    - `internal/plugin/storage/s3/` — S3-compatible
      (AWS / MinIO / Cloudflare R2 / etc.), including
      Object Lock for WORM and storage-class selection.
    Read both before writing your own; they're the
    de-facto extension of this contract.

## Interface

```go
// internal/plugin/storage/storage.go

package storage

type StoragePlugin interface {
    Name() string
    Open(ctx context.Context, cfg StorageConfig) error
    Put(ctx context.Context, key string, r io.Reader, opts PutOptions) (PutResult, error)
    Get(ctx context.Context, key string) (io.ReadCloser, error)
    Stat(ctx context.Context, key string) (ObjectInfo, error)
    List(ctx context.Context, prefix string) iter.Seq2[ObjectInfo, error]
    Delete(ctx context.Context, key string) error
    RenameIfNotExists(ctx context.Context, src, dst string) error
    SetRetention(ctx context.Context, key string, until time.Time, mode WORMMode) error
    Barrier(ctx context.Context) error // makes deferred Puts crash-durable
    Capabilities() Capabilities
    Close() error
}
```

All methods are **goroutine-safe** unless an implementation
documents otherwise.  Concurrent `Put` calls to the same
key are resolved by `IfNotExists` (only one wins) or by
last-writer-wins when `IfNotExists` is false.

## Lifecycle

```
   Register(scheme, factory)   ─ at init() time
              │
              ▼
   Open(ctx, StorageConfig)    ─ called once per process per repo
              │
   ┌──────────┼──────────┐
   ▼          ▼          ▼
  Put ─── Get ─── Stat ─── List ─── Delete ─── Rename ─── SetRetention
                              (in any order, concurrently)
              │
              ▼
   Close()                     ─ release backend connections
```

`Open` is idempotent; the host calls it once at repo
attach.  `Close` is idempotent; the host calls it once
at exit.

## Per-method contract

### `Name() string`

Lowercase backend name (`"fs"`, `"s3"`, `"azblob"`,
`"gcs"`, `"sftp"`).  Stable across versions; goes into
audit-log `subject.repo` fields and into the
`pg_hardstorage_storage_*` Prometheus metric labels.

### `Open(ctx context.Context, cfg StorageConfig) error`

Initialise the plugin.  `cfg.URL` is the canonical
address; `cfg.Extras` is a per-backend bag of extra
strings (region overrides, role-ARNs, SSE-KMS key IDs,
custom endpoints).  Idempotent on repeat calls — calling
`Open` twice with the same config is a no-op.

Errors: any backend-specific connection error.  Open
should NOT validate that the repo *exists*; that's
`pg_hardstorage repo init` / `repo open` territory.

### `Put(ctx, key, r, opts) (PutResult, error)`

Write `r` at `key`.  Either fully succeeds or the object
is not visible to subsequent operations — backends MUST
NOT leave half-written objects observable via `Get` or
`List`.

`PutOptions`:

| Field | Behaviour |
| --- | --- |
| `IfNotExists` | Atomically conditional.  Returns `ErrAlreadyExists` if the key is present.  **Required** for chunk dedup and manifest commit safety. |
| `ContentLength` | Optional pre-known size.  Lets backends pick optimal multipart strategy.  Zero means "unknown — buffer or stream as you can". |
| `ContentSHA256` | Expected SHA-256 of plaintext.  When non-zero, the plugin MUST verify after writing and return `ErrChecksumMismatch` on disagreement. |
| `StorageClass` | Backend-specific (S3: `STANDARD` / `STANDARD_IA` / `GLACIER` / etc.).  Empty = backend default. |
| `RetainUntil` | Per-object retention deadline.  Ignored when WORM unsupported. |
| `RetentionMode` | `WORMGovernance` (override permitted) or `WORMCompliance` (regulatory).  Empty implies Compliance. |
| `Metadata` | ASCII-keyed string map serialised as object metadata where supported. |

`PutResult` reports back what was committed.  Backends
that don't have a useful ETag may set it to the
lowercase-hex SHA-256.

Returns `ErrAlreadyExists` (when `IfNotExists` was true
and the key was present), `ErrChecksumMismatch`
(when `ContentSHA256` disagreed), or any backend
transport error.

### `Get(ctx, key) (io.ReadCloser, error)`

Returns a streaming reader.  **Caller closes.**  Returns
`ErrNotFound` when the key is absent.  Backends MUST NOT
buffer the entire object in memory — `Get` is on the hot
path of restore.

### `Stat(ctx, key) (ObjectInfo, error)`

Metadata only — size, mtime, ETag, storage class, version,
arbitrary metadata.  Returns `ErrNotFound` when absent.
`ContentSHA256` MAY be the zero value if the backend
doesn't expose a strong content hash.

### `List(ctx, prefix) iter.Seq2[ObjectInfo, error]`

Streams matching objects.  The iterator yields
`(info, nil)` per object and `(zero, err)` once on a
fatal listing error; consumers stop on either.  No
guaranteed ordering across backends.  Backends with
delimiter-based pagination (S3) MUST issue continuation
requests transparently.

### `Delete(ctx, key) error`

Removes `key`.  **Removing a non-existent key is a
no-op** — retried deletes (`pg_hardstorage repo gc`,
the audit-log replay) MUST be safe.  Do NOT return
`ErrNotFound` here.

### `RenameIfNotExists(ctx, src, dst) error`

Atomically renames `src` → `dst`, failing with
`ErrAlreadyExists` when `dst` is present.  This is the
manifest-commit primitive: `manifest.json.tmp` →
`manifest.json` is the moment a backup becomes visible.

Backends without native check-and-link semantics (most
object stores) MUST emulate it correctly:

1. `Stat(dst)` → if present, return `ErrAlreadyExists`.
2. Copy `src` → `dst` with `IfNotExists`.
3. `Delete(src)`.

The window between (1) and (2) is the only correctness
risk; the manifest writer is single-leader-per-backup so
the race is bounded.  Filesystems use `linkat(2) | rename(2)`
for true atomicity.

### `SetRetention(ctx, key, until, mode) error`

Apply a WORM retention deadline + lock posture.  Returns
`ErrUnsupported` when `Capabilities().WORM == false`.

`WORMCompliance` is the regulatory-grade default — once
set, no role inside the cloud account can shorten the
deadline (only "wait it out" works).  `WORMGovernance`
allows authorised override; do NOT use it for
SOC2 / HIPAA / FedRAMP backups.

### `Capabilities() Capabilities`

Advertises optional features:

```go
type Capabilities struct {
    WORM                   bool
    ConditionalPut         bool   // honours PutOptions.IfNotExists
    Multipart              bool   // accepts large streams without buffering
    ServerSideEncryption   bool
    CrossRegionReplicate   bool
    StorageClassSelectable bool
}
```

The host gates capability-dependent operations on these
flags.  Operations that *require* WORM (legal hold,
regulatory backups under `--worm-required`) refuse to
proceed when `WORM == false`; gracefully fall back when
optional.

### `Close() error`

Release backend connections / handles.  Idempotent.

## Optional interfaces

A `StoragePlugin` MAY additionally implement:

### `RegionAware`

```go
type RegionAware interface {
    Region() string  // "us-east-1", "eu-west-1", ...
}
```

Used by the residency-check pre-flight.  Plugins that
don't implement it (the `fs` plugin, where region isn't
meaningful) fall back to `RegionUnknown` ("") via the
`storage.RegionOf(plugin)` helper.

### `FreeSpaceAware`

```go
type FreeSpaceInfo struct {
    TotalBytes     int64
    AvailableBytes int64
    Unsupported    bool
}

type FreeSpaceAware interface {
    FreeSpace(ctx context.Context) (FreeSpaceInfo, error)
}
```

Used by the capacity pre-flight (refuses a backup whose
projected size exceeds the repo's free space).  Object
stores typically report `Unsupported: true`; the
filesystem plugin reports `statfs(2)` results.

Always consult these via the helpers
`storage.RegionOf(plugin)` and
`storage.FreeSpaceOf(ctx, plugin)`, not via direct
type-assertion — the helpers handle the
not-implemented case so callers branch on the value.

## Error sentinels

```go
var (
    ErrAlreadyExists    = errors.New("storage: object already exists")
    ErrNotFound         = errors.New("storage: object not found")
    ErrChecksumMismatch = errors.New("storage: checksum mismatch")
    ErrUnsupported      = errors.New("storage: capability not supported by backend")
    ErrUnknownScheme    = errors.New("storage: no plugin registered for scheme")
)
```

Use `errors.Is` for detection.  Implementations MUST wrap
their backend-specific errors with these sentinels so
upper layers don't leak SDK details — return
`fmt.Errorf("s3: %w: %s", storage.ErrNotFound, awsErr)`
not the raw SDK error.

## Registration

```go
// in your plugin's package
func init() {
    storage.Register("myproto", func() storage.StoragePlugin {
        return &MyPlugin{}
    })
}
```

A `Factory` returns a *fresh* plugin instance per call;
the host's `storage.Open(ctx, rawURL)` parses the URL,
looks up the factory by scheme, builds the plugin, calls
`Open(cfg)`, and returns the ready-to-use instance.

Schemes are matched by the URL's `Scheme` field, not by
prefix.  `s3://bucket/prefix` and `s3+https://...` are
two distinct schemes — register each one you accept.

Double-registration **panics**.  This is by design:
double-register is a programmer error, not a runtime
condition.

## Concurrency contract

| Operation | Concurrent calls allowed? |
| --- | --- |
| `Put` to different keys | Yes |
| `Put` to the same key | Yes — resolved by `IfNotExists` or last-writer-wins |
| `Get` to any key | Yes |
| `Get` while `Put` is in flight | Yes — `Get` MUST NOT see the in-progress write until it commits |
| `Delete` while `Get` is in flight | Yes — in-flight `Get` continues with the bytes it had at start |
| `Open` / `Close` | NO — single-threaded; the host serializes them |

## What backends MUST get right (the four invariants)

1. **`IfNotExists` is atomic.**  Two concurrent
   `Put(IfNotExists=true)` calls to the same key result in
   exactly one success; the other gets `ErrAlreadyExists`.
2. **Visibility is all-or-nothing.**  No reader EVER sees
   a partially-written object.
3. **`RenameIfNotExists` is atomic against `Get` and
   `Stat`.**  A reader who sees `dst` MUST see the full
   committed bytes; a reader who sees `ErrNotFound` for
   `dst` MUST not have seen partial bytes.
4. **`Delete` is idempotent.**  No `ErrNotFound` on a
   no-op delete.

Failure of any of these manifests as data-loss
corner cases.  Storage tests in
`internal/plugin/storage/storage_test.go` exercise all
four against any backend that implements this contract;
**run the table-driven test suite against your plugin**
before shipping.

## Tier-2 mapping

The Tier-2 gRPC contract for storage plugins (see
`proto/plugin/v1/plugin.proto` `service StoragePlugin`)
maps 1:1 to the Go interface above; the only real
difference is that `Get` is a streaming RPC
(`stream GetChunk`) whereas the in-process Go method
returns an `io.ReadCloser`.  See
[Tier-2 plugin protocol](tier2-go-plugin-protocol.md).

## Further reading

- Tutorial walk-through: see Phase 6's
  `tutorials/build-a-storage-plugin.md` once it lands.
- Reference URL schemes: `reference/storage-url-schemes.md`
  (auto-generated from `storage.Schemes()`).
- The four-invariant test harness:
  `internal/plugin/storage/storage_test.go`.
- WORM operational guidance: the `R4 — Repo corruption at
  rest` runbook.
