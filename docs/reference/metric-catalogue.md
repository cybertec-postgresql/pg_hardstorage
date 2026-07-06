<!-- AUTO-GEN candidate: scrape /metrics during `make docs-regen`; per docs/DOC_PLAN.md auto-generation map. -->
---
title: Metric catalogue
description: Prometheus metric names, labels, and types in the pg_hardstorage_ namespace.
tags:
  - reference
  - metrics
  - prometheus
  - observability
---

# Metric catalogue

The metric registry lives in
[`internal/obs/metrics`](https://github.com/cybertec-postgresql/pg_hardstorage/tree/main/internal/obs/metrics)
and is exposed in Prometheus text exposition format
(`text/plain; version=0.0.4`) at `/metrics`:

- on the **control plane**, always, unauthenticated (it shares
  the `/v1/*` listener);
- on the **agent**, when started with `--metrics-listen
  <host:port>` (the data-plane families are produced by the
  agent's backup / WAL / verify pipelines, so they live in the
  agent process).

!!! info "Live vs. reserved (SPEC drift #7)"
    The families marked **Live** below emit real samples today.
    The families marked **Reserved** have a committed name +
    label layout but no producer and no registration yet — they
    don't appear in the exposition at all, and are tracked
    as drift items #7/#8 in
    [`docs/SPEC_DRIFT.md`](../SPEC_DRIFT.md). Until a reserved
    signal lands, operators read it from the
    [audit log](../explanation/audit-chain.md) or the
    structured [`Event`](output-event-schema.md) stream.

## Conventions (committed)

- **Prefix:** every metric starts with `pg_hardstorage_`.
- **Units in the name:** `_seconds`, `_bytes`,
  `_total` for counters, plus `_bucket`/`_count`/`_sum`
  triples for histograms.
- **Cardinality cap:** `deployment` is the only operator-
  driven label everywhere; `tenant` joins `deployment` on
  multi-tenant deployments.  Free-form labels (backup ID,
  LSN, manifest path) **never** become labels — they go
  into events instead.

## Process / build — **Live**

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `pg_hardstorage_build_info` | gauge | `version`, `commit` | Always 1; carries the running binary's build metadata as labels. Set at startup by the control plane and by an agent with `--metrics-listen`. |

## Backup pipeline — **Live**

Produced by the backup runner; scrape from the agent (or any
process that runs `pg_hardstorage backup`).

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `pg_hardstorage_backup_started_total` | counter | `deployment`, `type` | Backups started, once PG is identified and the type is resolved (`full` / `incremental_lsn`). |
| `pg_hardstorage_backup_completed_total` | counter | `deployment`, `type`, `result` | Backups that reached a terminal state; `result` ∈ `success` / `failure`. |
| `pg_hardstorage_backup_duration_seconds` | histogram | `deployment`, `type` | Wall-clock duration of completed backups. |
| `pg_hardstorage_backup_bytes_logical` | gauge | `deployment` | Logical (pre-dedup, pre-compress) bytes in the latest backup. |
| `pg_hardstorage_backup_bytes_physical` | gauge | `deployment` | On-the-wire bytes the latest backup wrote to the repo (unique chunk bytes). |
| `pg_hardstorage_backup_dedup_ratio` | gauge | `deployment` | `physical / logical` for the latest backup (lower = better dedup). |
| `pg_hardstorage_chunk_uploads_total` | counter | `deployment`, `result` | CAS chunk outcomes; `result` ∈ `ok` (freshly written) / `dedup`. |

## WAL pipeline — **Live** (lag gauges reserved)

| Metric | Type | Labels | Meaning | Status |
| --- | --- | --- | --- | --- |
| `pg_hardstorage_wal_segments_archived_total` | counter | `deployment` | WAL segments archived to the repo (one per `wal push`). | Live |
| `pg_hardstorage_wal_archived_bytes_total` | counter | `deployment` | Logical WAL bytes archived (segments × 16 MiB). | Live |
| `pg_hardstorage_wal_archive_lag_seconds` | gauge | `deployment` | Lag between PG's flush LSN and the archived LSN. | Reserved |
| `pg_hardstorage_wal_archive_lag_bytes` | gauge | `deployment` | Same lag in bytes. | Reserved |

The "WAL archiving silence" alert pairs the live counter with
the reserved lag gauge:

```
rate(pg_hardstorage_wal_segments_archived_total[10m]) == 0
  and pg_hardstorage_wal_archive_lag_seconds > 600
```

## Verify — **Live**

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `pg_hardstorage_verify_runs_total` | counter | `deployment`, `result` | Verify runs that reached a verdict; `result` ∈ `success` / `failure` / `skipped`. |

## Restore — **Live**

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `pg_hardstorage_restore_started_total` | counter | `deployment` | Restores started. |
| `pg_hardstorage_restore_completed_total` | counter | `deployment`, `result` | Restores that reached a terminal state; `result` ∈ `success` / `failure`. |
| `pg_hardstorage_restore_duration_seconds` | histogram | `deployment` | Wall-clock duration of completed restores. |

## Replicate — **Live**

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `pg_hardstorage_replicate_runs_total` | counter | `result` | Cross-repo replicate runs; `result` ∈ `success` / `incomplete` / `failure`. |
| `pg_hardstorage_replicate_objects_copied_total` | counter | `kind` | Objects copied to the destination; `kind` ∈ `manifest` / `chunk` / `wal_manifest`. |
| `pg_hardstorage_replicate_bytes_copied_total` | counter | (none) | Bytes copied to the destination. |

## KMS — **Live**

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `pg_hardstorage_kms_unwrap_latency_seconds` | histogram | `scheme`, `result` | DEK-unwrap round-trip; `scheme` ∈ `local`, `aws-kms`, `gcp-kms`, `azure-kv`, `vault-transit`, `pkcs11`; `result` ∈ `success` / `failure`. |

## Control plane — **Live**

Served from the control plane's `/metrics`. `route` is folded
to a bounded set so high-cardinality path segments (job IDs,
deployment names) never become series.

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `pg_hardstorage_http_requests_total` | counter | `route`, `method`, `code` | Control-plane HTTP requests served. |
| `pg_hardstorage_http_request_duration_seconds` | histogram | `route` | Request handling latency. |
| `pg_hardstorage_jobs` | gauge | `state` | Jobs known to the control plane by state (`queued`/`running`/`completed`/`failed`/`cancelled`); idle states report 0. |
| `pg_hardstorage_agents` | gauge | `state` | Registered agents by liveness (`active`/`total`). |
| `pg_hardstorage_repos_configured` | gauge | (none) | Number of repositories the control plane serves. |
| `pg_hardstorage_controlplane_errors_total` | counter | `op` | Agent control-plane loop errors, by `op` (`heartbeat`/`claim`/`progress`/`complete`). |

## Repo — **Reserved**

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `pg_hardstorage_repo_bytes` | gauge | `repo`, `kind` | On-the-wire bytes the repo currently consumes. |
| `pg_hardstorage_repo_objects` | gauge | `repo`, `kind` | Object count in the repo. |

## Service-Level Indicators — **Reserved**

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `pg_hardstorage_rpo_seconds` | gauge | `deployment` | Observed RPO (lag between newest backup-recoverable point and now). |
| `pg_hardstorage_slo_rpo_target_seconds` | gauge | `deployment` | Operator-configured RPO target.  See [SPEC drift #8](../SPEC_DRIFT.md). |
| `pg_hardstorage_rto_estimate_seconds` | gauge | `deployment` | Estimated time to restore the latest full + WAL chain. |
| `pg_hardstorage_agent_up` | gauge | `deployment` | 1 when the agent's leader-follow loop is healthy. |
| `pg_hardstorage_leader_election_state` | gauge | `deployment` | Leader-election state (`0`=unknown, `1`=follower, `2`=leader). |

## Resilience — **Reserved**

The "did the system catch itself?" counters.  Steady-state
they sit at zero; non-zero is operationally interesting.

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `pg_hardstorage_resilience_chunk_retries_total` | counter | `deployment` | CAS-chunk retries (transient backend errors). |
| `pg_hardstorage_resilience_circuit_breaker_open_total` | counter | `deployment`, `target` | Circuit-breaker openings (storage / KMS / PG). |
| `pg_hardstorage_resilience_backpressure_seconds_total` | counter | `deployment` | Cumulative time the pipeline back-pressured. |
| `pg_hardstorage_resilience_failover_handled_total` | counter | `deployment` | Patroni failovers detected and absorbed. |
| `pg_hardstorage_resilience_slot_recreated_total` | counter | `deployment`, `node` | Replication slots auto-recreated post-failover. |
| `pg_hardstorage_resilience_manifest_repaired_total` | counter | `deployment` | Manifests repaired through `repair manifest`. |
| `pg_hardstorage_resilience_scrub_findings_total` | counter | `deployment` | Findings emitted by scrub. |
| `pg_hardstorage_resilience_gameday_runs_total` | counter | `deployment` | Disaster-drill runs. |
| `pg_hardstorage_resilience_panic_total` | counter | `subsystem` | Recovered Go panics. |

## Audit — **Reserved**

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `pg_hardstorage_audit_anchor_lag_seconds` | gauge | (none) | Time since the last audit-anchor commit. |

## Anomaly detection — **Reserved**

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `pg_hardstorage_anomaly_score` | gauge | `deployment`, `metric` | Z-score over the rolling baseline; alerts at `> 3`. |

## LLM — **Reserved**

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `pg_hardstorage_llm_tokens_total` | counter | `provider`, `direction` | Tokens consumed; `direction` ∈ `prompt` / `completion`. |

## See also

- [Operations: monitoring](../operations/monitoring.md) —
  scraping recipes and Grafana dashboard JSON.
- [Operations: alerting recipes](../operations/alerting-recipes.md) —
  Prometheus rules built on these names.
- [Operations: SLO as code](../operations/slo-as-code.md) —
  how RPO targets become declarations.
- [SPEC drift](../SPEC_DRIFT.md) — items #7 and #8 track the
  reserved-name families.
