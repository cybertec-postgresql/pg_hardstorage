# pg/streaming/

Shared receive-loop wiring: the `CopyBothResponse`/`XLogData` reader plumbing
that both physical and logical receivers sit on top of.

## What lives here

The lowest-level streaming primitives — message framing, keepalive cadence,
`StandbyStatusUpdate` writer, context-cancel propagation — extracted so
`../replication/` (physical) and `../logicalreceiver/` (logical) can share them
without copy-paste. This is intentionally tiny: anything message-type-specific
lives in the caller.

## Key files

- `reader.go` — `Reader` over a `pgconn.PgConn` in CopyBoth mode;
  iterator-style `Next()` returning the next decoded message (or keepalive /
  standby-deadline trigger)
- `reader_test.go` — frame parsing, keepalive timing, cancel-during-read edge
  cases

## Keepalive semantics

The PG primary sends `PrimaryKeepalive` messages when no WAL is pending; the
reader surfaces these so the caller can decide whether to reply with a
`StandbyStatusUpdate` (acknowledging `flushedLSN` / `appliedLSN`). The reader
also tracks the standby-status deadline so the caller never starves the primary
of progress reports — a starved primary disconnects after
`wal_sender_timeout`.

## Cancellation

`ctx` cancellation cuts the underlying read inside one network round-trip;
partially-decoded messages are discarded. The caller is responsible for resuming
from the last acknowledged LSN, not the last decoded one.

## Read next

- `../replication/README.md` — physical caller
- `../logicalreceiver/README.md` — logical caller
- `../../wal/README.md` — the eventual home of received WAL

## Don't put X here

- Message-type semantics — keep the layer dumb; callers decide what `XLogData`
  *means*.
- Slot management — that's in the caller package.
- Backup orchestration — `internal/backup/` and `internal/wal/`.
