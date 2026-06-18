---
title: Capacity planning
description: Project repository growth at 30/90/365-day horizons with pg_hardstorage capacity report.
tags:
  - capacity
  - planning
---

# Capacity planning

`pg_hardstorage capacity report` projects repository size
at a chosen horizon by fitting a linear least-squares trend
to the manifest history. It is the operator's answer to
"how much storage do I need to provision for the next
quarter?"

The model is honest about its limits: bursty workloads get
a low R² and a `low` / `insufficient` confidence label
rather than a misleadingly precise projection.

---

## Lead with the command

```sh
pg_hardstorage capacity report --repo s3://acme-backups/
pg_hardstorage capacity report --repo s3://acme-backups/ --horizon 720h
pg_hardstorage capacity report --repo s3://acme-backups/ --horizon 30d -o json
```

Default horizon is `90d`. Override with `--horizon` (Go
duration syntax — `24h`, `720h` for 30 days, the `<N>d`
shorthand is also accepted).

---

## What you get

```console
capacity report — s3://acme-backups/
  Generated:    2026-04-28T14:21:08Z
  Horizon:      90d (at 2026-07-27T14:21:08Z)
  Confidence:   high
  Samples:      247
  Current:      1.4 TB
  Per day:      18.2 GB
  Projected:    3.0 TB (Δ 1.6 TB)
  Fit (R²):     0.987

  Per deployment:
    DEPLOYMENT  BACKUPS  CURRENT   PER-DAY  PROJECTED  R²
    db1         142      820 GB    11.4 GB  1.8 TB     0.991
    db2         105      540 GB    6.8 GB   1.1 TB     0.974
```

The fields are stable per the v1 schema. The JSON form is
the same shape; pipe `-o json` into `jq` for dashboard
ingest.

---

## Confidence labels

| Label | Meaning |
| --- | --- |
| `high` | R² ≥ 0.9. Linear fit explains > 90% of variance. |
| `medium` | 0.7 ≤ R² < 0.9. Use the projection but expect moderate error. |
| `low` | 0.4 ≤ R² < 0.7. Bursty growth or a step change recently. Re-run after the burst settles. |
| `insufficient` | Fewer than 3 manifest commits in the deployment's history. No projection possible. |

Confidence is also fleet-wide: the global slope is a
weighted combination of each deployment's slope, weighted
by current footprint. A single deployment with a
quarterly-import burst can drag down a fleet whose other
deployments are smooth — investigate the per-deployment
table when the global confidence is `low` and per-deployment
ones are `high`.

---

## When to use which horizon

| Horizon | Use case |
| --- | --- |
| 30d | Sprint planning, monthly budget review. |
| 90d | Default. Quarterly capacity review, S3 budget approval. |
| 365d | Hardware procurement, multi-year retention budget. |

For 365d horizons on small fleets, validate against actual
disk-bandwidth growth — at the 1-year scale, secondary
factors (table growth, retention policy changes, dedup
ratio drift) accumulate enough to dwarf the linear trend.

---

## Hooking into capacity in CI / monitoring

The structured JSON form is suitable for daily monitoring:

```sh
pg_hardstorage capacity report --repo s3://acme-backups/ -o json \
    | jq -e '.result.body.confidence == "high"
             and .result.body.projected_bytes < 5e12'
```

Exit code is non-zero (`jq -e`) if the projection breaches
5 TB or confidence drops to `medium`. Wire into a daily cron
that posts to Slack via the
[sink configuration](operator-guide.md#10-sinks) when the
breach fires.

---

## Pre-flight: capacity preflight

Before scheduling a large incoming deployment, run
`capacity preflight` to confirm the existing repo can absorb
a new growth profile without exceeding budget:

```sh
pg_hardstorage capacity preflight \
    --repo s3://acme-backups/ \
    --projected-bytes 500000000000 \
    --safety-factor 1.2
```

Returns a structured "yes/no/marginal" verdict against the
projected total. Useful in onboarding tickets — it answers
"can we host this customer in the existing repo?" without
provisioning anything.

---

## Caveats

- The projection uses **logical** (pre-dedup) bytes for the
  deployment slice and **physical** (post-dedup) bytes for
  the global total. The two diverge as dedup ratio shifts;
  the report always shows both.
- **Retention policy changes** invalidate the trend.
  Re-baseline by running `--ignore-before <date>` after the
  retention policy stabilises (v0.5+).
- **Compression posture changes** (zstd → lz4) compact the
  on-disk footprint but don't change logical bytes; the
  report's per-deployment slice is unaffected, but the
  global physical projection bends.

---

## Further reading

- [Cost reporting](cost-reporting.md) — same data, billing
  view.
- [Monitoring](monitoring.md) — `pg_hardstorage_repo_*`
  metrics for live capacity dashboards.
- [Operator guide: retention](operator-guide.md#3-retention)
  — how policy changes feed into capacity projections.
