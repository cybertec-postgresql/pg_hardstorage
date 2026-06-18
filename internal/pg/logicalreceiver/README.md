# pg/logicalreceiver/

The logical-decoding receiver: drive `START_REPLICATION ... LOGICAL` with the
`pgoutput` plugin, hand decoded changes to a sink.

## What lives here

`Stream(ctx, conn, opts, sink)` and the slot lifecycle around it. Used for
change-data-capture flows (selective replication, audit streams, downstream
replication to non-PG targets) — not for taking backups, which go through
`../basebackup/` + `../walsink/`. The output protocol is `pgoutput` (the in-tree
publication-aware decoder); we never link against a contrib plugin.

## Key files

- `stream.go` — the receive loop: `START_REPLICATION SLOT <name> LOGICAL
  <startLSN> (proto_version '4', publication_names '<pubs>')`, message decoding,
  `StandbyStatusUpdate` flow control
- `slot.go` / `slot_integration_test.go` — `CreateLogicalSlot`,
  `DropLogicalSlot`, snapshot import, plugin name validation
- `logicalreceiver_test.go` — table-driven message decode coverage (`Begin`,
  `Commit`, `Insert`, `Update`, `Delete`, `Truncate`, `Relation`, `Type`,
  `Origin`)
- `integration_test.go` — end-to-end against a real PG with a publication

## Notes

- Requires `wal_level=logical` (validated by the replication preflight).
- Two-phase commit messages (`Begin Prepare`, `Prepare`, `Commit Prepared`,
  `Rollback Prepared`) are pgoutput protocol v3+.
- Streamed in-progress transactions (proto v2+) and parallel apply (proto v4)
  are handled transparently to the sink.

## Read next

- `../replication/README.md` — sibling, physical streaming
- `../streaming/README.md` — shared receive-loop wiring
- `../../wal/README.md` — physical WAL (separate path; not consumed here)
- `../../backup/README.md` — backups never go through this package

## Don't put X here

- Physical WAL — that's `../replication/` + `../walsink/`.
- Decoder plugins other than `pgoutput` — out of scope.
- Sink wire transports — sinks are passed in; their plumbing lives in the
  consumer.
