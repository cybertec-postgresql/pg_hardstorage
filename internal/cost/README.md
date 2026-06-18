# cost/

Per-deployment / per-repo cost report: where is the storage budget going, and
what is it costing per month?

## What lives here

Walks the repo and reports:

- Total physical bytes (chunks + manifests + WAL + audit)
- Per-deployment manifest bytes
- Per-deployment WAL bytes (`wal/<deployment>/...`)
- Per-deployment logical bytes (sum of `FileEntry.Size` from committed manifests
  — pre-dedup, pre-compression)
- Estimated monthly cost = `total_physical * price_per_gb_month`

Per-deployment *chunk* bytes are deliberately approximate — chunks are
content-addressed and shared via dedup; a precise per-deployment chunk
attribution would require walking the full reference graph and apportioning
multi-referenced chunks. The honest v0.1 cut reports total chunk bytes once,
separately.

## Key files

- `cost.go` — `Report`, repo walk, per-deployment aggregation, monthly-cost
  arithmetic
- `cost_test.go` — fixture-driven roll-up correctness

## Read next

- `../capacity/README.md` — sibling: point-in-time capacity gate + projection
- `../forecast/README.md` — sibling: long-horizon trend projection (also
  drives cost-over-time)
- `../README.md` — parent index

## Don't put X here

- Real-time billing API integrations — out of scope; operator passes a rate.
- Multi-tier S3 / Glacier breakdown — v0.1 reports total physical; tier-aware
  accounting is a follow-up.
