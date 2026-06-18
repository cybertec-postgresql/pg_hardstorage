# logical/

Logical-decoding orchestrator: one supervised pipeline per registered stream,
three pluggable sinks.

## What lives here

A pipeline is the tuple `(deployment, stream-name, slot, plugin, publication,
sink)`. The `Manager` state file at `paths.State()/logical_streams.json` is the
registry (`logical add` appends, `logical list` walks, `logical remove`
deletes). The `Runner` supervises per-stream goroutines, restarts on transient
errors, and reports lag.

Lag is computed from committed segment manifests + `pg_replication_slots`, so
the answer is durable + monitorable from outside the running agent.

The receive layer (replication-slot wire protocol, WAL message parsing) lives at
`internal/pg/logicalreceiver/`; this package is the orchestrator on top of it.

## Sinks

- `sinks/chunked/` (default) — CAS-backed, per-batch manifests, idempotent on
  retry
- `sinks/s3events/` — emit per-event objects to an S3 bucket (or any
  S3-compatible store)
- `sinks/webhook/` — POST per-event to an HTTP endpoint (with retries +
  backoff)

## Key files

- `orchestrator.go` — `Manager`, registry state file, add/list/remove
- `runner.go` — supervised per-stream goroutines, restart loop, hot-reload
- `lag.go` — committed-segment + slot-lag computation
- `runner_*_test.go` — hot-reload, shutdown, integration coverage
- `lag_test.go` / `lag_integration_test.go` — lag arithmetic against real
  slots

## Read next

- `../pg/logicalreceiver/` — the wire-protocol layer this orchestrator runs on
  top of
- `../pg/walsink/` — sibling physical WAL pipeline (different shape, same
  supervised-goroutine pattern)
- `../README.md` — parent index

## Don't put X here

- PG-side replication-slot wire code — that's `internal/pg/logicalreceiver/`.
- Output plugin selection at the PG side — operators set `wal2json` /
  `pgoutput` on the publication; this package consumes whatever the slot emits.
