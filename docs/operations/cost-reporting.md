---
title: Cost reporting
description: Per-deployment and per-tenant repository cost via pg_hardstorage cost report.
tags:
  - cost
  - billing
---

# Cost reporting

`pg_hardstorage cost report` walks the repository and
returns bytes consumed by category, with a monthly-USD
estimate. The report is the input for the operator's
"where is my repo budget going?" question and for the
billable-export workflow per-tenant.

---

## Lead with the command

```sh
pg_hardstorage cost report --repo s3://acme-backups/
pg_hardstorage cost report --repo s3://acme-backups/ \
    --price-per-gb-month 0.0125 \
    -o json
```

Default price is `$0.023/GB-month` — AWS S3 Standard,
us-east-1, posted Q2 2026. Override with
`--price-per-gb-month` for other backends or contracts.

---

## What you get

```console
cost report — s3://acme-backups/
  Total physical:  1.4 TB
    chunks:        1.2 TB
    manifests:     8.4 GB
    wal:           187 GB
    audit:         312 MB
  Estimated:       $32.79 / month  (at $0.023/GB-month)

  Per deployment:
    DEPLOYMENT  BACKUPS  LOGICAL   MANIFESTS  WAL
    db1         142      4.2 TB    4.7 GB     112 GB
    db2         105      2.8 TB    3.7 GB     74 GB
```

The physical total is exact across all categories. The
per-deployment logical bytes are pre-dedup, pre-compression
— useful for SLA / billable footprint reporting (the
"customer's logical data") but distinct from physical
bytes paid to the storage backend.

---

## Why per-deployment chunk attribution is approximate

Chunks are content-addressed. Two deployments backing up
the same template database share the underlying chunks —
which is the dedup feature. A precise per-deployment chunk
attribution requires walking the full reference graph and
apportioning multi-referenced chunks. v0.1 reports total
chunk bytes once at the repo level; v0.5+ ships the
reference-graph walker for chargeback billing.

The per-deployment slice always exposes:

- **Backup count** — exact.
- **Logical bytes** — sum of `FileEntry.Size` across every
  committed manifest. Pre-dedup, pre-compression.
- **Manifest bytes** — exact (manifest file sizes).
- **WAL bytes** — exact (`wal/<deployment>/...` prefix).

For billing pipelines that need a per-deployment chunk
allocation today, divide the global chunk bytes by the
sum of deployment logical bytes — that's the rough average
post-dedup ratio per logical byte. The error term is the
cross-deployment dedup overlap, which is small for
production fleets.

---

## Per-tenant exports

A tenant boundary in `pg_hardstorage` is a logical grouping
of deployments under a shared KEK. The report splits per
tenant when the deployments are tagged:

```sh
pg_hardstorage cost report --repo s3://acme-backups/ \
    -o json
```

Returns one row per tenant in the result body. The KEK
namespace alignment makes tenant-level chunk attribution
exact: chunks under tenant T's KEK never dedup against
tenant U's chunks (different per-chunk keys), so per-tenant
chunk bytes is the sum of chunks encrypted under T's
wrapped DEK.

JSON shape (excerpt):

```json
{
  "schema": "pg_hardstorage.cost.v1",
  "by_tenant": [
    {
      "tenant": "acme-prod",
      "deployments": ["db1", "db2"],
      "physical_bytes": 1389567800832,
      "estimated_monthly_usd": 29.78
    },
    {
      "tenant": "acme-staging",
      "deployments": ["db1-staging"],
      "physical_bytes": 64236000000,
      "estimated_monthly_usd": 1.38
    }
  ]
}
```

---

## What's not in v0.1

- **Multi-cloud price tables.** Azure, GCS, on-prem rates
  vary per region + storage class. The single
  `--price-per-gb-month` flag is the v0.1 escape hatch; pass
  the contract rate from your billing system.
- **Time-windowed slicing.** Cost trends need historical
  sampling, which the SLO + capacity reports collect. v0.5+
  ships a time-series-backed `cost trend --since 90d`.
- **Egress / KMS API costs.** The report is on-disk
  storage only. Egress for restore + replication, KMS
  unwrap-per-restore, and S3 LIST request costs are
  workload-dependent and not included.

---

## Hooking into billing pipelines

The JSON form is stable per the v1 schema. A typical
chargeback pipeline:

```sh
pg_hardstorage cost report --repo s3://acme-backups/ \
    -o json \
    | jq -c '.result.body.by_tenant[] |
        {tenant, physical_gb: (.physical_bytes / 1e9),
         monthly_usd: .estimated_monthly_usd}' \
    | curl --data-binary @- https://billing.acme.example.com/api/v1/usage
```

Schedule as a daily cron via the agent's
[scheduler](operator-guide.md#11-configuration), or invoke
on-demand from the billing system.

---

## Reconciliation

The `repo usage` and `cost report` commands read the same
underlying object listing and must always agree. A drift
between them is a bug — file an issue with both outputs
attached.

```sh
pg_hardstorage repo usage --repo s3://acme-backups/ -o json \
    > usage.json
pg_hardstorage cost report --repo s3://acme-backups/ -o json \
    > cost.json
jq -s '.[0].result.body.bytes_total - .[1].result.body.total_physical_bytes' \
    usage.json cost.json
# Expect: 0
```

---

## Further reading

- [Capacity planning](capacity-planning.md) — same data,
  forward-looking projection.
- [Operator guide: repo usage](operator-guide.md#5-repository-management)
  — the operational view of the same numbers.
- [Compliance: data residency](../compliance/data-residency-pinning.md)
  — region pinning interacts with cost (egress, multi-region
  storage).
