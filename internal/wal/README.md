# wal/

WAL-side coordination: who's the leader to stream from, what segments are
already archived, where the gaps are, and which timelines we've seen.

## What lives here

Four siblings, each a narrow concern. The actual 16 MiB segment-writer lives one
floor down at `internal/pg/walsink/` — this package coordinates around it.

## Key files / subdirs

- `follower/` — Patroni leader-follow coordinator
  - `coordinator.go` — watches Patroni, picks a replica/leader to stream from,
    manages slot continuity across switchover
  - `multislot_test.go`, `replica_pick_test.go` — fan-out + replica-selection
    logic
- `gapstate/` — typed gap records persisted under `wal/<deployment>/gaps/`
  - `gapstate.go` — `Gap` record, persistence, query
  - `purge_test.go` — gap-record retention
- `inventory/` — archive inventory
  - `inventory.go` — exposes `HighestArchivedLSN` and per-timeline coverage
    for restore preflight
- `timeline/` — timeline-history capture
  - `timeline.go` — capture `*.history` files into the repo so restore can
    replay across promotions
  - `worm_propagation_test.go` — verifies WORM mode survives a timeline write

## Where the actual sink lives

WAL segment bytes are written by `internal/pg/walsink/` — it owns the receive
loop, the manifest, and the push into CAS. This package answers "where should we
stream from?" and "what's missing?", not "how do we write a segment?".

## Read next

- `../pg/walsink/` — the segment writer
- `../pg/replication/` — physical replication primitives that `follower/`
  drives
- `../patroni/README.md` if present — HTTP client used by
  `follower/coordinator.go`
- `../restore/gapcheck.go` — consumer of `inventory/` and `gapstate/` during
  restore preflight

## Don't put X here

- The actual segment writer — that's `internal/pg/walsink/`.
- BASE_BACKUP code — that's `internal/pg/basebackup/`.
- CAS path layout — that's `internal/repo/layout.go`.
