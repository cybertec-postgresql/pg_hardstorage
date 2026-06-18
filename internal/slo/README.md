# slo/

Per-deployment RPO / RTO declarations as code, with a report that compares
actual last-backup-age against the declared RPO.

## What lives here

The unit of declaration is a YAML `SLO` block per deployment:

```yaml
deployment: db1
rpo: 1h    # max acceptable data loss (last backup must be newer than this)
rto: 4h    # max acceptable time-to-restore (drill-derived)
```

The report walks every deployment's most-recent committed backup, computes `now
- StoppedAt`, and compares against `rpo`. RTO is sourced from
`internal/recovery/drill.go` history. Result is a pass/fail-per-deployment
scorecard with a single fleet-wide verdict.

Status: skeleton — the index entry under `internal/README.md` notes this is a
stub. The surface is defined; the wiring lands alongside the next compliance /
recovery iteration.

## Key files

To be added when the implementation lands. Expected:

- `slo.go` — `SLO`, `Report`, parser, comparison arithmetic
- `markdown.go` — renderer for the scorecard

## Read next

- `../recovery/README.md` — RTO data source (drill history)
- `../forecast/README.md` — projects when an RPO target will start being
  missed at current growth
- `../compliance/README.md` — SLO scorecards contribute evidence to
  availability controls
- `../README.md` — parent index
