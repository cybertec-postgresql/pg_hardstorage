# anomaly/

Dependency-free baseline + z-score detector for backup-shape outliers (size,
duration, file count, page churn).

## What lives here

Pure math: no storage, no manifests, no PG. Callers feed `Sample`s and read
`Report`s. The algorithm is intentionally boring — compute mean and stddev
over the most-recent N priors of the same deployment + type, score `(x - mu) /
sigma`, flag when `|score|` exceeds threshold. Seasonal / ARIMA-style modelling
is deliberately out — backup metrics aren't seasonal in a way that pays for
the cleverness.

## Key files

- `detector.go` — `Sample`, `Baseline`, `Report`, `Score`, `Detect`
- `detector_test.go` — small-sample edge cases, sigma == 0, outlier
  classification

## Read next

- `../insider/README.md` — same statistical philosophy applied to audit-event
  patterns (sibling of this package)
- `../forecast/README.md` — forward projection of size / WAL; consumes the
  same manifest stream
- `../capacity/README.md` — pre-flight capacity gate, also manifest-driven
- `../README.md` — parent index

## Don't put X here

- Manifest walking — the detector is fed `Sample`s; the caller (CLI / agent)
  does the walking.
- Audit-event scoring — that's `internal/insider/`.
