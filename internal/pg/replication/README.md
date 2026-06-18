# pg/replication/

Physical-replication primitives: replication slots and the `START_REPLICATION`
streaming loop that feeds the WAL sink.

## What lives here

The wire-protocol layer for physical streaming replication. Slot CRUD
(`CreatePhysicalSlot`, `CreatePhysicalSlotReserveWAL`, `DropSlot`, `GetSlot`),
the `Stream()` loop that consumes `XLogData` and emits keepalives, the
slot-continuity guard that prevents archive gaps after a slot recreation, and
the preflight checks that refuse to start on a misconfigured server.

## Key files

- `slot.go` / `slot_integration_test.go` — slot CRUD against
  `pg_replication_slots`
- `stream.go` / `stream_test.go` — `Stream(ctx, conn, slot, startLSN, sink)`:
  drives `START_REPLICATION`, parses `XLogData` and primary-keepalive messages,
  sends `StandbyStatusUpdate`
- `continuity.go` / `continuity_test.go` / `gap_integration_test.go` —
  `EnsureSlot`: compares the slot's `restart_lsn` against the WAL we last
  committed; aborts if the server has discarded WAL we haven't archived
- `preflight.go` / `preflight_test.go` / `preflight_integration_test.go` —
  `wal_level`, `max_wal_senders`, slot-availability, role-privilege checks
- `integration_test.go` — end-to-end against a real PG

## Read next

- `../walsink/README.md` — receive-side: assembles 16 MiB segments from
  `XLogData`
- `../streaming/README.md` — shared receive-loop wiring
- `../../wal/README.md` — orchestration of WAL collection
- `../../patroni/README.md` — HA-aware slot continuity in failover

## Slot reservation

`CreatePhysicalSlotReserveWAL` issues `CREATE_REPLICATION_SLOT ... PHYSICAL
RESERVE_WAL` so the server pins WAL from slot creation onward. Use this before
kicking off a base backup that pairs with a slot — otherwise the server can
recycle WAL we still need.

## Don't put X here

- Logical decoding — that's `../logicalreceiver/`.
- Segment assembly / on-disk layout — that's `../walsink/`.
- Manifest writing — `internal/wal/` handles segment manifests.
