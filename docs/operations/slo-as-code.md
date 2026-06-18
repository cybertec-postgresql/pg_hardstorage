---
title: SLO as code
description: Declarative RPO/RTO targets per deployment with pg_hardstorage slo set / show.
tags:
  - slo
  - rpo
  - rto
---

# SLO as code

`pg_hardstorage` declares Recovery Point Objective (RPO)
and Recovery Time Objective (RTO) per deployment. The
declaration lives in the deployment config, the report
compares actual against target, and the alerting layer
fires on misses.

SLOs are advisory — a missed RPO surfaces as a finding in
the report, not an exit-code failure. Operators wanting
hard gates wire the report's JSON output into their
monitoring tool's alert ladder (see
[alerting recipes](alerting-recipes.md#backup-overdue)).

---

## Lead with the command

```sh
pg_hardstorage slo set db1 --rpo 1h --rto 30m
pg_hardstorage slo set db2 --rpo 6h
pg_hardstorage slo show
pg_hardstorage slo report
```

`slo set` writes to `pg_hardstorage.yaml`. `slo show` reads
the configured targets. `slo report` compares each
deployment's latest backup `StoppedAt` against the target.

---

## Duration syntax

Both Go-style and the `<N>d` shorthand are accepted:

| Input | Meaning |
| --- | --- |
| `30m` | 30 minutes |
| `1h` | 1 hour |
| `24h` | 24 hours |
| `7d` | 7 days (shorthand; Go's stdlib doesn't support `d` natively, but the binary does) |

---

## Configuring per deployment

Direct YAML edit (drop-in `conf.d/`) instead of the CLI:

```yaml
deployments:
  db1:
    pg_connection: postgres://pgbackup@db1.example.com/postgres
    repo: s3://acme-backups/
    slo:
      rpo_seconds: 3600    # 1h
      rto_seconds: 1800    # 30m
```

The CLI form normalises to `_seconds` on disk; either
shape works.

---

## What the report looks like

```console
slo report
  Evaluated:    2026-04-28T14:21:08Z

  DEPLOYMENT  RPO TARGET  RPO ACTUAL  STATUS  NOTE
  db1         1h          47m         met
  db2         6h          7h12m       missed  RPO target 6h; actual 7h12m (1h12m over)
  db3         —           —           no_target
  db4         —           —           no_backups
  db5         —           —           no_repo
```

Statuses:

| Status | Meaning |
| --- | --- |
| `met` | Latest backup is within RPO target. |
| `missed` | Latest backup is older than RPO target. |
| `no_target` | Deployment has no RPO/RTO declared. |
| `no_backups` | No committed manifests for this deployment. |
| `no_repo` | Deployment has no `repo` configured. |
| `error` | Repo open / list failed; the row's `note` carries the error string. |

---

## Wiring into alerts

The metric layer publishes the configured target as
`pg_hardstorage_slo_rpo_target_seconds{deployment}` and the
observed RPO as `pg_hardstorage_rpo_seconds{deployment}`.
Compare in PromQL:

```promql
pg_hardstorage_rpo_seconds
  > on (deployment) group_left
  pg_hardstorage_slo_rpo_target_seconds
```

The full alert rule is in
[alerting-recipes.md#backup-overdue](alerting-recipes.md#backup-overdue).

For deployments without a target, fall back to a fleet
threshold (24h is a reasonable global default).

---

## RTO measurement

RTO actuals are populated by the verifier subsystem (v0.5+).
The verifier runs sandbox restores periodically; the
elapsed time is correlated with the declared RTO target and
flagged when over.

For v0.1, RTO is informational — the target is recorded but
the actual is `—`. Wire `recovery drill` into a scheduled
job for measured RTO:

```sh
pg_hardstorage recovery drill <deployment>
```

The drill records actual RTO + writes an audit event
(`recovery.drill_completed`) the SLO report can consume.

---

## SLO report from CI

The structured JSON form is suitable for daily CI:

```sh
pg_hardstorage slo report -o json \
    | jq -e '[.result.body.deployments[]
              | select(.status == "missed")]
             | length == 0'
```

Exit non-zero when any deployment misses its RPO. Pair with
a daily cron + a Slack sink for on-call visibility.

---

## Realistic targets

Common starting points:

| Workload class | RPO target | RTO target |
| --- | --- | --- |
| Critical OLTP | 5m–15m | 5m–15m |
| Standard production | 1h–6h | 30m–1h |
| Reporting / analytics | 12h–24h | 4h–24h |
| Dev / staging | 24h–72h | best-effort |

The RPO target sets the **schedule cadence floor** —
backups must run at least every `RPO`. The RTO target sets
**capacity requirements** — bandwidth + parallelism +
sandbox count must be enough to hit `RTO` from cold.

---

## SLO drift

A deployment whose actual RPO has been creeping toward the
target deserves attention before it misses. The `report`
output includes the actual RPO so trend-tracking is
straightforward:

```sh
pg_hardstorage slo report -o json \
    | jq '.result.body.deployments[] |
          {deployment, target: .rpo_target,
           actual: .rpo_actual,
           headroom: (.rpo_target - .rpo_actual)}'
```

Push to your monitoring backend; alert on `headroom < 0.2 *
target` (i.e. actuals approaching 80% of target).

---

## Caveats

- A single missed schedule (an agent restart, a network
  blip) flips the status to `missed` for the next report,
  even if the prior week's backups were healthy. The
  alerting layer's `for: 5m` window absorbs the transient.
- `slo report` does NOT trigger a backup; it's a read-only
  observation. Combine with `pg_hardstorage backup` in the
  same script if you want the report to also kick off a
  catch-up backup when missed.
- The target lives on the deployment config, not the repo.
  A repo backing two deployments with different targets
  reports each independently.

---

## Further reading

- [Alerting recipes: backup overdue](alerting-recipes.md#backup-overdue)
  — PromQL for SLO breach.
- [Monitoring](monitoring.md) — `pg_hardstorage_rpo_seconds`
  and the SLO target metrics.
- [Incident response](incident-response.md) — what to do
  when an SLO breach fires at 3am.
- [Recovery drills](operator-guide.md#4-verification) — RTO
  measurement.
