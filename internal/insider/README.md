# insider/

Insider-threat anomaly scoring on top of the hash-chained audit log: unusual
download patterns, novel IAM principals, off-hours bulk reads, first-seen tenant
crossings.

## What lives here

A *baseline window* (e.g. prior 30 days) defines normal — which actors do
what, in which tenants, at which hours. A *target window* (e.g. last 24 h) is
scored against the baseline. Findings are produced when something in the target
breaks the baseline pattern. Every rule is auditable and explainable (no opaque
ML); the operator gets a reason they can replay against the audit chain.

Scans are themselves recorded under `insider/scans/<id>.json`, so future commits
can layer threshold attestations on a scan ("no findings as of T") to bless a
release.

## Key files

- `insider.go` — `Scan`, `Finding`, rule evaluation, baseline + target window
- `insider_test.go` — first-seen-actor, off-hours bulk, tenant-crossing,
  destructive-action rules

## Read next

- `../anomaly/README.md` — sibling: same statistical philosophy applied to
  backup metrics
- `../audit/README.md` — the data source; every event the scanner reads is
  hash-chained
- `../threshold/README.md` — multi-party blessing of a clean scan
- `../README.md` — parent index

## Don't put X here

- Backup-shape outliers — that's `internal/anomaly/`.
- Live ingestion / streaming — the scanner walks the audit chain on demand.
