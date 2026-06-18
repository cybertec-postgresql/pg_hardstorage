# capacity/

30 / 90 / 365-day projections of repo size, chunk count, WAL volume — and the
pre-flight free-space gate that fires before every backup.

## What lives here

Walks every committed manifest's `StartedAt` + size, fits a least-squares line
to (timestamp, cumulative bytes), and extrapolates to the requested horizon.
R-squared rides along so the operator knows how much to trust the projection.
Refuses to project with fewer than 3 data points (returns a structured
"insufficient history" result instead of a noisy line through two points).

`preflight.go` is the pre-backup gate: estimates the backup's footprint and
refuses to start when destination free space is below a configurable margin.

## Key files

- `capacity.go` — `Project`, linear-fit arithmetic, R-squared, horizon
  enumeration
- `preflight.go` — pre-backup capacity gate; reads filesystem free space
- `capacity_test.go` / `preflight_test.go` / `preflight_integration_test.go` —
  fit correctness, gate behaviour, end-to-end

## Read next

- `../forecast/README.md` — sibling: fleet-level long-horizon projection
  (overlaps; capacity is point-in-time, forecast is multi-horizon)
- `../cost/README.md` — sibling: turns projected bytes into projected $$$
- `../anomaly/README.md` — companion outlier detector on the same manifest
  stream
- `../README.md` — parent index

## Don't put X here

- Non-linear models (logistic / piecewise / seasonal) — those land alongside a
  future time-series store shared with SLO + cost.
- Cloud-pricing arithmetic — that's `internal/cost/`.
