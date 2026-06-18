# recovery/

Recovery toolkit: readiness scorecards, recovery-window enumeration, drill
runner, and step-by-step restore-runbook generation.

## What lives here

Two CLI-facing surfaces and an offline drill runner:

- **readiness** — "if I had to recover this deployment right now, would it
  actually work, and how long would it take?". Aggregates latest backup age,
  verification freshness, KEK reachability, WAL coverage into a single scorecard
  + traffic-light verdict.
- **windows** — every committed backup, with the PITR window it anchors.
  Surfaces WAL-coverage gaps that break PITR.
- **drill** — end-to-end restore drill into a verifier sandbox; signs a
  `DrillReport` for evidence.
- **history** — past drill results from the audit chain.
- **runbook generator** — step-by-step Markdown runbook per (deployment,
  scenario).

Read-only by construction (except the drill, which restores into a disposable
sandbox). Safe against a WORM-locked repo.

## Key files

- `readiness.go` — scorecard aggregation, traffic-light verdict
- `windows.go` — PITR-window enumeration from manifest + WAL inventory
- `drill.go` — drill orchestration; restores into `internal/verify/sandbox`
- `history.go` — past drill results from audit chain
- `markdown.go` — runbook renderer (`pg_hardstorage.recovery.drill.v1`)
- `wal_inventory.go` — WAL coverage probe (wired to `wal/inventory`)
- `recovery_test.go` / `drill_test.go` / `drill_integration_test.go` —
  end-to-end drill, scorecard, history

## Read next

- `../gameday/README.md` — sibling: scripted chaos drills (different shape,
  same goal)
- `../compliance/README.md` — recovery evidence contributes to BCP / DR
  controls
- `../verify/sandbox/` — the disposable PG sandbox the drill restores into
- `../wal/inventory/` — the WAL-coverage data source
- `../README.md` — parent index

## Don't put X here

- Production restore — that's `internal/restore/`; this package only drills +
  reports.
