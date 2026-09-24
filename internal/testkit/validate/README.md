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

## Verdict

A run passes only if no failure was recorded. Every failure goes through
`recordFailure`, which also emits a `cell_failed` event (the Pushgateway
`pg_hardstorage_validate_pass` gauge is driven by exactly that event, so it
agrees with the report). Failure kinds:

| Kind | Raised when |
| --- | --- |
| `setup`, `seed`, `sustained_load`, `wal_stream` | the cell could not start |
| `backup`, `verify` | pg_hardstorage failed a backup / restore-verify on a live cell |
| `recovery` | a fault was applied and could not be reverted — the testbed's failure, but nothing measured on that cell afterwards can be trusted |
| `cell_down` | the cell went `--max-backup-gap` (default 1h) without a backup getting through, or none of its dispatched backups ever got through |
| `retention` | rotate / gc failed, or one repository's gc was deferred `--retention-max-deferrals` (default 4) windows in a row |

Skips are not failures: `backup_skipped_cell_down`, `verify_skipped_cell_down`,
`fault_skipped_cell_down`, `fault_skipped_limit_unreachable`,
`fault_skipped_not_applicable` and a single `retention_deferred` are the testbed
racing its own faults. The `cell_down` floor is what stops a cell that a fault
killed for good from passing on skips alone. `fault_apply_failed` is counted
(`fault_apply_fails`) but is not a failure by itself: some catalogues expect an
injector to refuse (`inode_exhaustion`).

Detection is not failure either: `backup_refused_source_corruption` (PostgreSQL
refusing a torn page) and `verify_refused_injected_corruption` (a restore
refusing a backup that predates a repo-corruption fault this cell injected)
count as `corruption_detected`. Repo-corruption faults (`manifest_targeted_corruption`,
`truncated_wal_segment`, `missing_wal_segment`) are confined to the injecting
cell's deployment (`inject.Registry.ApplyForDeployment`), so they cannot damage
— and blame — another cell sharing the repository.

Fault reverts run with their own bounded context, so a fault in flight at the
run deadline is still reverted before teardown.

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
