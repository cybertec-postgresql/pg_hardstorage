# runner

The step dispatcher — runs a parsed scenario end-to-end: bring up the
topology, run the load, walk each step, evaluate the assertions, tear down.

## What lives here

`runner.go` owns the lifecycle (topology Up, load apply, scenario asserts,
Down). `steps.go` owns the per-step-kind handlers (`take_backup`, `restore`,
`run_load`, `capture_lsn`, `assert_restored_match`, `wal_stream`, `switch_wal`,
`inject`, `cli_run`, …). Cross-step state — repo URL, deployment name, last
produced backup ID, agent-binary path, the topology's inject targets — flows
through a `runState` threaded into each handler so a `restore` step can default
to the most recent `take_backup`. The runner is the test-side mirror of the
agent's job dispatch and is deliberately small and inspectable; we expect
operators to read it while debugging a flaky scenario.

## Key files / subdirs

- `runner.go` — main loop + `runState`
- `steps.go` — handlers for the core data-path step kinds
- `steps_l4.go` — heavier L4-tier handlers (long-chain restore, etc.)
- `cli_run.go` — generic shell-out to `pg_hardstorage` and compat shims via
  the `shim:` field; closes the gap for the 40+ operational subcommands without
  their own step kind, with placeholder substitution for scenario state
- `ops.go` — repo-init + helper operations shared across steps
- `restored_load.go` — re-running the load against a restored PG for
  byte-equality verification
- `corrupt_repo.go` — deliberate-corruption helpers used by negative-path
  scenarios
- `compat_archive.go`, `compat_doppelganger.go` — Barman/pgBackRest/WAL-G
  compat-shim drivers
- `runner_integration_test.go`, `cli_run_test.go`, `steps_test.go` — unit +
  integration tests

## Read next

- `../scenario/README.md` — the YAML the runner consumes
- `../assert/README.md` — the DSL the runner evaluates at each `assert:` step
- `../inject/README.md` — what `inject:` step kinds do
- `../topology/README.md` — providers the runner brings Up/Down

## Don't put X here

No YAML parsing — that's `scenario/`. No fault-application logic — that's
`inject/`. No assertion semantics — those live in `assert/`. The runner is the
dispatcher; the verbs live elsewhere.
