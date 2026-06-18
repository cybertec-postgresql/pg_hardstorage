# pg/

The Postgres wire-layer toolkit. Every byte that crosses a libpq replication
connection passes through code in here.

## What lives here

BASE_BACKUP protocol, physical START_REPLICATION + slot lifecycle, logical
START_REPLICATION + slot management, the 16 MiB WAL-segment sink, the shared
receive loop, mode-aware connection wrapping, IDENTIFY_SYSTEM, timeline-history
capture, and a containerised PG fixture for integration tests.

## Key files / subdirs

- `conn.go` — `Conn` wrapper: replication-vs-normal mode selection, timeout
  enforcement
- `identify.go` — `IDENTIFY_SYSTEM` parser (system id, timeline, XLogPos)
- `timeline.go` — fetch + parse `*.history` files
- `version.go` — `server_version_num` probe
- `basebackup/` — `BASE_BACKUP` driver including incremental (UPLOAD_MANIFEST
  + `INCREMENTAL true`)
- `replication/` — physical: `stream.go` receive loop, `slot.go` lifecycle,
  `continuity.go` + `EnsureSlot` for failover gap-detection, `preflight.go`
  gates
- `logicalreceiver/` — logical: `stream.go`, `slot.go`, plus a `Sink`
  interface so callers plug in their own consumer
- `walsink/` — writes the 16 MiB segment files into the repo (`push.go`,
  `manifest.go`, `walsink.go`)
- `streaming/` — `reader.go`: the message-pump shared by physical + logical
  paths
- `testkit/` — `pg.go` spins up a real Postgres container for integration
  tests

## Read next

- `../backup/README.md` — calls `basebackup` to drive BASE_BACKUP
- `../wal/README.md` — consumes what `walsink` writes
- `../logical/README.md` — wires a real consumer to `logicalreceiver.Sink`

## Don't put X here

- Manifest-level logic (signing, chaining) — that's `internal/backup/`.
- Repo I/O (CAS keys, GC) — that's `internal/repo/`.
- Patroni leader-follow coordination — that's `internal/wal/follower/`.
