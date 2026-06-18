# forecast/

Capacity + cost projections from historical telemetry. Answers "given how the
fleet has grown over the last N days, where will we be in 30 / 90 / 365 days,
and what will it cost?".

## What lives here

Linear regression on observable points (manifest `StoppedAt` + logical bytes,
WAL volume) produces a slope (bytes/day, manifests/day) and R-squared for
confidence reporting. Sparse-data deployments get "low confidence" + a note, not
a noisy line through two points. Cost projection is opt-in via
`--price-per-gb-month` — we never try to look up cloud pricing automatically.
Read-only by construction; safe against a WORM-locked repo.

Different from neighbouring surfaces:

- `doctor` — present-state host / config / connectivity health
- `repo audit` — present-state inventory
- `compliance` — historical events rollup
- `forecast` — historical trends → forward projection

## Key files

- `forecast.go` — `Report`, linear regression, R-squared, horizon arithmetic
  (~1000 LOC)
- `markdown.go` — `Render` to Markdown for capacity reviews
- `forecast_test.go` — slope correctness, R-squared edge cases, low-confidence
  behaviour

## Read next

- `../capacity/README.md` — sibling: per-backup pre-flight capacity gate
  (point-in-time)
- `../cost/README.md` — sibling: per-deployment cost breakdown (point-in-time)
- `../anomaly/README.md` — backup-shape outlier detector with the same
  manifest-stream source
- `../README.md` — parent index

## Don't put X here

- Cloud-pricing lookups — operator passes a rate; this package projects.
- Real-time alerting — this is a planning surface, not a monitoring one.
