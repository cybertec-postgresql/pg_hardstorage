---
title: Alerting recipes
description: Drop-in PromQL and Alertmanager rules for backup overdue, WAL silence, KEK unreachable, and scrub findings.
tags:
  - alerting
  - prometheus
  - alertmanager
---

# Alerting recipes

Drop-in alert rules for the metrics in
[monitoring.md](monitoring.md). Every rule below is paired
with the runbook it points at — keep the runbook URL in the
annotation so the on-call paste leads to a known good page.

---

## Backup overdue

Fires when a deployment's last successful backup is older
than the configured RPO target. The
[SLO-as-code](slo-as-code.md) layer publishes a
deployment-keyed `pg_hardstorage_rpo_seconds` metric; the
alert compares observed lag against the target.

```yaml
- alert: HSBackupOverdue
  expr: pg_hardstorage_rpo_seconds > on (deployment) group_left
        pg_hardstorage_slo_rpo_target_seconds
  for: 5m
  labels:
    severity: warning
  annotations:
    summary: "{{ $labels.deployment }}: backup overdue"
    description: |
      Last backup completed {{ $value | humanizeDuration }} ago,
      RPO target is {{ with query (printf "pg_hardstorage_slo_rpo_target_seconds{deployment=\"%s\"}" $labels.deployment) }}
      {{ . | first | value | humanizeDuration }}{{ end }}.
    runbook: docs/runbooks/R3-cold-start-from-backups.md
```

For deployments without an SLO target, fall back to a
fleet-wide threshold:

```yaml
- alert: HSBackupOverdueGlobal
  expr: pg_hardstorage_rpo_seconds > 86400
  for: 15m
  labels:
    severity: warning
  annotations:
    summary: "{{ $labels.deployment }}: no backup in 24h"
```

---

## WAL silence

A WAL pipeline that stops shipping is the single most common
RPO-violating failure mode. The pipeline normally emits at
least one segment every 5 minutes (the `archive_timeout`
default + the agent's `Standby Status Update` cadence). A flat
counter is the symptom.

```yaml
- alert: HSWALSilence
  expr: rate(pg_hardstorage_wal_segments_archived_total[10m]) == 0
        and pg_hardstorage_wal_archive_lag_seconds > 600
  for: 5m
  labels:
    severity: critical
  annotations:
    summary: "{{ $labels.deployment }}: WAL pipeline silent"
    description: |
      No segments archived in 10m and lag is
      {{ $value | humanizeDuration }}. The replication slot
      may be dropped, or the agent's connection is wedged.
    runbook: docs/runbooks/R6-slot-dropped-gap.md
```

The 10-minute rate window guards against a slow-traffic
deployment where a single 16 MiB segment fills slowly — those
backups are still healthy as long as `archive_timeout` is
flushing periodic empty segments.

---

## Anomaly score elevated

`pg_hardstorage_anomaly_score` is a Z-score over the rolling
30-day distribution of each kind. A score above 3 means the
latest backup deviates by more than three standard deviations
from baseline.

```yaml
- alert: HSAnomalyHigh
  expr: pg_hardstorage_anomaly_score > 3
  for: 0m
  labels:
    severity: warning
  annotations:
    summary: "{{ $labels.deployment }}: anomalous {{ $labels.kind }}"
    description: |
      Backup deviates {{ $value | printf "%.1f" }}σ from the
      30-day baseline on the "{{ $labels.kind }}" axis.
      Investigate before the next scheduled backup.
```

The three kinds carry different operational meanings:

- `kind="size"` — sudden volume drop or jump (table dropped /
  bulk import / replication broken).
- `kind="churn"` — chunk dedup ratio shifted (workload changed
  or the index broke).
- `kind="duration"` — slower backup than usual (storage
  latency / CPU pressure / contention).

Dispatch differently per kind in your Alertmanager routing
config; the runbook for each lives in
[incident response](incident-response.md).

---

## Scrub findings

`repair scrub` (and the in-repo `integrity` package) emit
findings each run. A nonzero count is bit rot we caught
before restore.

```yaml
- alert: HSScrubFindings
  expr: increase(pg_hardstorage_resilience_scrub_findings_total[24h]) > 0
  for: 0m
  labels:
    severity: critical
  annotations:
    summary: "{{ $labels.kind }} finding(s) on the repository"
    description: |
      The integrity scrub surfaced
      {{ $value }} {{ $labels.kind }} finding(s) in the last 24h.
      Quarantine before restoring; heal from replica region.
    runbook: docs/runbooks/R4-repo-corruption-at-rest.md
```

`kind` is `bit-rot` (chunk SHA mismatch), `missing` (manifest
references a chunk not in CAS), or `orphan` (chunk in CAS
referenced by no live manifest).

`orphan` findings are not corruption — they're GC backlog;
route at `severity: info` if your alerting policy splits.

---

## KEK unreachable

`kms_unwrap_latency_seconds` tail latency is the canonical
KMS-degradation symptom. Pair with the
`kms.unreachable` audit event for the binary "did it fail"
signal.

```yaml
- alert: HSKEKUnreachable
  expr: histogram_quantile(0.95,
          rate(pg_hardstorage_kms_unwrap_latency_seconds_bucket[5m])
        ) > 5
  for: 5m
  labels:
    severity: critical
  annotations:
    summary: "KMS p95 unwrap latency > 5s"
    description: |
      KMS provider is degraded. Backups will fail; restores
      cannot decrypt. Check the provider console + IAM trail.
    runbook: docs/runbooks/R2-kms-key-destroyed.md
```

A complete KMS outage usually surfaces as
`pg_hardstorage_kms_unwrap_latency_seconds_count` flatlining
at zero — backups stop committing entirely. The Layer-7
symptom is `backup.failed{reason="kms_unreachable"}` in the
audit chain.

---

## Verify failures

```yaml
- alert: HSVerifyFailures
  expr: increase(pg_hardstorage_verify_runs_total{result="failed"}[1h]) > 0
  for: 0m
  labels:
    severity: critical
  annotations:
    summary: "{{ $labels.deployment }}: verification failed"
    description: |
      Tier "{{ $labels.tier }}" verification failed. The
      affected backup is documented in the audit chain
      ({{ $labels.deployment }}).
    runbook: docs/runbooks/R4-repo-corruption-at-rest.md
```

A `result="failed"` on `tier="fast"` (signature + chunk SHA
round-trip) means encryption or storage corruption.
A `result="failed"` on `tier="full"` (sandbox restore +
`pg_verifybackup`) means the backup is unrestorable — even
if its individual chunks are intact.

---

## Resilience: panics + circuit breakers

```yaml
- alert: HSPanic
  expr: increase(pg_hardstorage_resilience_panic_total[1h]) > 0
  for: 0m
  labels:
    severity: critical
  annotations:
    summary: "{{ $labels.component }}: panic captured"
    description: |
      A panic was caught and the supervisor restarted the
      worker. Investigate the stderr log around the event.

- alert: HSCircuitBreakerOpen
  expr: pg_hardstorage_resilience_circuit_breaker_open_total > 0
  for: 5m
  labels:
    severity: warning
  annotations:
    summary: "{{ $labels.backend }}: circuit breaker open"
    description: |
      Backend rejected enough requests that the circuit
      breaker opened. The agent will back off and retry.
```

---

## Agent down

```yaml
- alert: HSAgentDown
  expr: pg_hardstorage_agent_up == 0
  for: 5m
  labels:
    severity: critical
  annotations:
    summary: "{{ $labels.agent }}: agent unreachable"
    runbook: docs/operations/troubleshooting.md
```

The `agent_up` gauge flips to zero only when the local
supervisor's heartbeat to the control plane stops. A scrape
failure on `/metrics` itself is what your Prometheus
`up{job="pg_hardstorage"} == 0` rule catches; pair both.

---

## Routing

A reasonable Alertmanager `route` tree:

```yaml
route:
  receiver: ops-default
  group_by: [alertname, deployment]
  routes:
    - matchers: [severity="critical"]
      receiver: ops-pager
      continue: true
    - matchers: [alertname=~"HSAnomalyHigh"]
      receiver: ops-slack
      group_interval: 1h    # anomalies don't need to page on the half-hour
    - matchers: [alertname="HSCircuitBreakerOpen"]
      receiver: ops-slack
```

For deployments using `pg_hardstorage`'s own
[sinks](operator-guide.md#10-sinks), the `pagerduty`,
`opsgenie`, and `jira` plugins are alternatives to running
Alertmanager — they emit per-event without a Prometheus
intermediary.

---

## Further reading

- [Monitoring](monitoring.md) — the full metric catalog.
- [Incident response](incident-response.md) — what to do when
  one of these fires at 3am.
- [Runbook index](../reference/runbooks/index.md) — R1–R7
  full incident playbooks.
