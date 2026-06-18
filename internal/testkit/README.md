# testkit

The testkit is pg_hardstorage's scenario- and soak-test engine — the Go
libraries the `pg_hardstorage_testkit` binary (under
`cmd/pg_hardstorage_testkit/`) drives.

## What lives here

Scenarios live as YAML at `test/scenarios/*.scenario.yaml`; load definitions at
`test/load/*.load.yaml`. The packages below parse those files, bring up PG
topologies and storage sinks, run the load, inject faults, and evaluate the
assertion DSL. The runner is the test-side mirror of the agent's job dispatch.

## Key files / subdirs

- `runner/` — step dispatcher, the scenario main loop
- `scenario/` — YAML schema for `pg_hardstorage.scenario.v1`
- `load/` — deterministic load engine (chacha20-seeded PRNG)
- `inject/` — fault-injection registry + targeting
- `sink/` — test-side runtimes for storage backends (MinIO, Azurite, fake-gcs,
  SFTP)
- `topology/` — PG infrastructure providers (local-docker,
  patroni-local-docker, stubs)
- `assert/` — declarative assertion DSL kinds
- `validate/` — long-running soak orchestrator (`testkit validate` subcommand)
- `bisect/` — scenario-aware git-bisect helper
- `mutation/` — mutation-testing build-tag harness
- `catalog/` — OS × PG-version × architecture × operator catalog
- `random/`, `prng/`, `imagetag/`, `coverage/`, `report/`, `reproducer/`,
  `config/`, `compose/`, `watch/` — leaf utilities

## Read next

- `cmd/pg_hardstorage_testkit/` — the CLI that wires these packages together
- `test/scenarios/README.md` — the scenario corpus + tier conventions
- `test/load/README.md` — load-file authoring rules

## Don't put X here

No production code paths. Anything the agent or server uses at runtime lives
under `internal/agent/`, `internal/server/`, `internal/backup/`, etc. —
testkit is allowed to depend on those packages, never the reverse.
