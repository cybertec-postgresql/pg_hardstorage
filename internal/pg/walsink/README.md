# pg/walsink/

The receive-side of physical WAL: assemble 16 MiB segments from `XLogData`
messages, chunk them, commit a per-segment manifest atomically.

## What lives here

The sink that `pg/replication.Stream()` writes into during continuous archiving,
and the `wal push` handler that `archive_command` invokes for synchronous
server-side archiving. Both paths end in the same place: a chunked, encrypted
segment in the repo plus a signed per-segment manifest that the WAL store can
replay deterministically.

## Key files

- `walsink.go` — `Sink` implementation; segment boundary detection from
  `XLogRecPtr`, partial-segment handling on disconnect, segment finalisation
- `manifest.go` — per-segment manifest (timeline, segno, LSN range, chunk
  digests, signature)
- `push.go` / `push_test.go` — `PushSegmentFile(ctx, segPath)` for
  `archive_command`; auxiliary files (`.history`, `.partial`, `.backup`) handled
  with their own paths
- `worm_propagation_test.go` — guards that WORM / object-lock retention
  propagates from segment to repo
- `integration_test.go` — end-to-end against a real PG with both streaming and
  `archive_command`

## Atomicity contract

A segment manifest commits only after every chunk has hit the storage plugin's
durable surface and the manifest store's `RenameIfNotExists` succeeds. Crash
mid-flight leaves at most a partial segment that the next run discards; never a
manifest pointing at missing chunks.

## Read next

- `../replication/README.md` — the upstream stream
- `../streaming/README.md` — shared receive-loop wiring
- `../../wal/README.md` — outer orchestrator and reader-side
- `../../backup/README.md` — sibling pipeline, shares chunker + repo

## Don't put X here

- Wire-protocol parsing — that's `../replication/`.
- WAL replay / restore — `internal/wal/` + `internal/restore/`.
- Logical decoding output — `../logicalreceiver/`.
