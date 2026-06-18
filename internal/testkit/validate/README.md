# validate

The soak-driver orchestrator — what the `pg_hardstorage_testkit validate`
subcommand wraps. Despite the name, this is not a scenario validator (scenarios
have their own pre-flight); this package runs one iteration loop per cell
concurrently for the configured duration.

## What lives here

The orchestrator calls into a `CellRuntime` to drive load, take backup, verify,
and apply faults, so the loop is testable against a fake runtime without
touching real PG or Docker. Real soak runs use `DockerCellRuntime` (host-mapped
to a docker-compose PG, drives load via pgx, shells out to `pg_hardstorage` for
backup + restore); tests construct `FakeCellRuntime`, which records every call
and lets the test simulate failures.

Workload shape is picked from a small set of `Schema` generators referenced by
name in `profiles.yaml`:

- `tpcc-lite` — mixed read/write OLTP
- `bulk-copy` — sequential heavy writes streaming a single growing fact table
- `schema-churn` — frequent `ALTER TABLE` on top of a tpcc-lite shape

The runtime calls `SchemaSetup` once and `SchemaIteration` each loop. Metrics
emit to a Prometheus Pushgateway so a long-running soak feeds a dashboard.

## Key files / subdirs

- `orchestrator.go` — `Run`, `RunOptions`, the per-cell concurrent loop
- `cellruntime.go` — `CellRuntime` interface + package doc
- `runtime_docker.go` — real-PG runtime via docker-compose + pgx
- `runtime_fake.go` — in-memory fake for unit tests
- `schemas.go` — workload-shape generators (`tpcc-lite`, `bulk-copy`,
  `schema-churn`)
- `pushgateway.go` — Prometheus Pushgateway emitter for soak metrics
- `orchestrator_test.go`, `runtime_docker_test.go`, `pushgateway_test.go`,
  `schemas_test.go`, `export_test.go` — unit tests + export hooks

## Read next

- `cmd/pg_hardstorage_testkit/validate.go` — CLI wiring
- `../inject/README.md` — fault catalogue the soak loop picks from
- `../report/` — soak-run report types this orchestrator emits

## Don't put X here

No scenario-step semantics — those live in `runner/`. Soak runs are loops over
a generic schema, not scripted scenarios.
