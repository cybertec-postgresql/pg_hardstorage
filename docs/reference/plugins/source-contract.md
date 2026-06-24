---
title: Source plugin contract
description: The SourcePlugin interface (forward-looking) — what pg_hardstorage backs up from.
tags:
  - plugins
  - source
  - reference
---

# Source plugin contract

A Source plugin tells `pg_hardstorage` *how to read from
PostgreSQL*: streaming-backup (the v0.1 default,
`pg_basebackup`-style), `pgincr17` (block-level
incremental built on PG 17's incremental backup feature),
or filesystem snapshot (LVM, ZFS, EBS, GCP PD, Azure
managed-disk).

!!! warning "Forward-looking contract"
    The Source tier is **not yet refactored into a
    pluggable interface**.  v1.0 ships a single
    streaming-backup implementation in
    `internal/backup/runner/` whose orchestration is hard-
    coded.  The interface signature below is the SPEC's
    target shape (see `SPEC.md` "Plugin model"); the
    refactor lands in v1.0.

    Tier-2 source plugins go through the same gRPC
    protocol as the other tiers — see
    `proto/plugin/v1/plugin.proto`
    `enum PluginTier::PLUGIN_TIER_SOURCE` (reserved; no
    service rpcs yet).

## Interface (target)

```go
// internal/plugin/source/source.go (planned v1.0)

package source

type SourcePlugin interface {
    Name() string
    Capabilities() SourceCapabilities
    Prepare(ctx context.Context, target PGTarget) (SourceSession, error)
}

type SourceCapabilities struct {
    Full        bool   // can produce a full backup
    Incremental bool   // can produce an incremental backup against a parent
    Snapshot    bool   // can produce a filesystem-snapshot backup
    Streaming   bool   // streams chunks vs. requires a finalised tarball
}

type PGTarget struct {
    DSN            string
    Version        int       // 17 = "pg17"
    SystemID       string    // PG system identifier
    DataDirectory  string    // for snapshot sources
}

type SourceSession interface {
    StartLSN() string
    Files() iter.Seq2[FileEntry, error]
    StopLSN() string
    Close() error
}

type FileEntry struct {
    Path    string
    Size    int64
    Reader  io.Reader   // chunk-stream of file contents
}
```

## Capabilities

The four capability flags drive backup orchestration:

| Capability | Meaning |
| --- | --- |
| `Full` | Source can produce a self-contained full backup with no parent. |
| `Incremental` | Source can produce a backup tied to a parent (delta against a previous backup or a `--since-lsn`). |
| `Snapshot` | Source produces an instantaneous point-in-time view via filesystem-level snapshotting (LVM/ZFS/EBS/etc.). |
| `Streaming` | Source emits file content as a chunk stream rather than producing a final tarball that must then be uploaded. |

The runner's strategy:

1. Compute the desired backup *shape* from CLI flags
   (full vs incremental, parent backup ID).
2. Pick the source whose `Capabilities()` intersect with
   the desired shape.
3. `Prepare` the session, walk `Files()`, emit chunks
   into the CAS.
4. Capture `StartLSN` / `StopLSN` for the manifest.
5. Close the session (releases backend resources — DDL
   locks for snapshot sources, replication slots for
   streaming).

## Reference implementations (current, pre-refactor)

The hard-coded streaming-backup logic lives in:

- `internal/backup/runner/runner.go` — the orchestration
- `internal/pg/basebackup/` — the `pg_basebackup` wire
  protocol shim
- `internal/pg/walsink/` — WAL streaming (separate from
  base backup; see the WAL tier)

The pgincr17 (block-level incremental) logic is gated
behind PG 17 detection in the same runner.  When the
plugin refactor lands, these become three implementations
of `SourcePlugin`:

- `internal/plugin/source/streaming/` (Tier-1)
- `internal/plugin/source/pgincr17/` (Tier-1)
- `internal/plugin/source/snapshot/` (Tier-1)

…plus Tier-2 binaries for vendor-specific block-level
shims (Patroni callbacks, custom replication slots,
proprietary CDC).

## What plugin authors will need to know (when the refactor lands)

1. **`Prepare` is the heavy method.**  Authentication,
   replication slot creation, snapshot LVM-freeze
   handshake, all happen here.  `Files()` iteration MUST
   be efficient.
2. **`StartLSN` and `StopLSN` are the manifest's
   timeline anchors.**  They MUST be the LSNs that
   bracket the file-system view emitted by `Files()` —
   PITR replay depends on them.
3. **`SourceSession.Close` releases the slot.**  Failure
   to close leaks WAL on the source PostgreSQL.

## Status

| Component | Status |
| --- | --- |
| Interface signature | TARGETED — see SPEC.md "Plugin model" |
| Tier-1 streaming-backup as a plugin | Planned v1.0 |
| Tier-1 pgincr17 as a plugin | Planned v1.0 |
| Tier-1 snapshot as a plugin | Planned v1.0 |
| Tier-2 gRPC service for SOURCE | Reserved enum value; no RPCs in proto v1 |
| Cross-version PG (PG 14+) source matrix | Tracked in `test/matrix.yaml` |

## Further reading

- `SPEC.md` "Plugin model" — the v1.0 target shape.
- `internal/backup/runner/` — current pre-refactor
  implementation.
- `proto/plugin/v1/plugin.proto` — the Tier-2 contract
  (Source RPCs to be added in v1.1 protocol).
