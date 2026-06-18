---
title: Monitoring
description: Prometheus scrape, OpenTelemetry traces, structured-JSON logs, and the pg_hardstorage_* metric namespace.
tags:
  - monitoring
  - prometheus
  - opentelemetry
  - logs
---

# Monitoring

`pg_hardstorage` ships three observability surfaces, each
independently optional and each emitting under a stable
contract:

| Surface | Endpoint / sink | Schema |
| --- | --- | --- |
| Prometheus metrics | `/metrics` on the control plane (always) and the agent (`--metrics-listen`) | `pg_hardstorage_*` namespace |
| OpenTelemetry tracing | OTLP/HTTP exporter | `pg_hardstorage.span.v1` |
| Structured logs | stderr (JSON) and any configured log sink | `pg_hardstorage.log.v1` |

Audit events are NOT logs — they go through the audit chain
described in [the operator guide](operator-guide.md#8-audit-log)
and are exported as
[evidence bundles](../compliance/audit-evidence-bundles.md).

---

## Prometheus

Two processes expose `/metrics`, both rendering the
`pg_hardstorage_` namespace in Prometheus text exposition
format (`text/plain; version=0.0.4`):

- **Control plane** — always on, served from the same listener
  as the `/v1/*` REST API and **unauthenticated** (like
  `/healthz`/`/readyz`), so a scraper needs no operator bearer
  token. It carries control-plane state: HTTP-request counters,
  job counts by state, agent liveness, configured-repo count,
  and build info.
- **Agent** — opt-in. Start the agent with
  `--metrics-listen <host:port>` (empty disables it; a loopback
  bind such as `127.0.0.1:9187` is the safe default). The chosen
  address is announced in the agent's event stream at launch.
  It carries the data-plane families the agent's pipelines
  produce: backup, WAL-archive, verify, KMS, and chunk-upload
  metrics.

```yaml
scrape_configs:
  - job_name: pg_hardstorage_control_plane
    scrape_interval: 30s
    metrics_path: /metrics
    static_configs:
      - targets: ["control.example.com:8443"]

  - job_name: pg_hardstorage_agents
    scrape_interval: 30s
    metrics_path: /metrics
    static_configs:
      - targets:
          - agent-db1.example.com:9187   # requires `agent --metrics-listen :9187`
          - agent-db2.example.com:9187
```

All metrics live in the `pg_hardstorage_` namespace. The labels
are stable per the v1 contract: a script written against the
first shipped metric keeps working forward.

!!! tip "Try it with the eval stack"
    The repo's `docker compose up` evaluation stack wires this end
    to end: the agent runs with `--metrics-listen 0.0.0.0:9187`, a
    Prometheus service scrapes it
    ([`deploy/compose/prometheus.yml`](https://github.com/cybertec-postgresql/pg_hardstorage/blob/main/deploy/compose/prometheus.yml)),
    and Grafana boots with the Prometheus datasource and a
    *pg_hardstorage overview* dashboard already provisioned. After
    `docker compose up`, `curl http://localhost:9187/metrics` returns
    the live namespace and Grafana at `http://localhost:3000`
    (`admin`/`admin`) shows the backup/WAL panels. Remember the agent
    listener is **opt-in** — an agent started without
    `--metrics-listen` serves no `/metrics` at all.

!!! note "Live now vs. reserved names"
    The registry lives in
    [`internal/obs/metrics`](https://github.com/cybertec-postgresql/pg_hardstorage/tree/main/internal/obs/metrics).
    The families that **emit real data today** are: the backup
    pipeline counters/gauges, the `restore_*` counters/histogram,
    the `replicate_*` counters, `chunk_uploads_total`,
    `wal_segments_archived_total` + `wal_archived_bytes_total`,
    `verify_runs_total`, `kms_unwrap_latency_seconds`, the
    control-plane `http_requests_total` /
    `http_request_duration_seconds` / `jobs` / `agents` /
    `repos_configured`, `controlplane_errors_total`, and
    `build_info`. The remaining tables
    below (`wal_archive_lag_*`, `repo_objects`/`repo_bytes`, the
    `resilience_*` namespace, `anomaly_score`, `rpo_seconds` /
    `rto_estimate_seconds`, `agent_up` /
    `leader_election_state`, `llm_tokens_total`) are **reserved
    names** — the layout is committed but the producers haven't
    landed yet (tracked as drift #7/#8 in
    [`docs/SPEC_DRIFT.md`](../SPEC_DRIFT.md)). They render as
    `# HELP`/`# TYPE` headers with no samples until then.

### Pipeline counters

| Metric | Labels | Type |
| --- | --- | --- |
| `pg_hardstorage_backup_started_total` | `deployment`, `type` | counter |
| `pg_hardstorage_backup_completed_total` | `deployment`, `type`, `result` | counter |
| `pg_hardstorage_backup_duration_seconds` | `deployment`, `type` | histogram |
| `pg_hardstorage_backup_bytes_logical` | `deployment` | gauge |
| `pg_hardstorage_backup_bytes_physical` | `deployment` | gauge |
| `pg_hardstorage_backup_dedup_ratio` | `deployment` | gauge |
| `pg_hardstorage_chunk_uploads_total` | `result` (`ok`/`dedup`/`error`) | counter |

`type` is `full` or `incremental_lsn`; `result` for
`backup_completed_total` is `success` or `failure`.
`chunk_uploads_total`'s `result` is `ok` (freshly written) or
`dedup`.

### WAL

| Metric | Labels |
| --- | --- |
| `pg_hardstorage_wal_segments_archived_total` | `deployment`, `mode` (`stream`/`library`/`cmd`) |
| `pg_hardstorage_wal_archive_lag_seconds` | `deployment` |
| `pg_hardstorage_wal_archive_lag_bytes` | `deployment` |

`wal_archive_lag_seconds` is the wall-clock age of the most
recent confirmed-flush LSN. It is the SLO-correlated metric —
wire it directly into the `wal_silence` alert rule (see
[alerting recipes](alerting-recipes.md#wal-silence)).

### Repository

| Metric | Labels |
| --- | --- |
| `pg_hardstorage_repo_objects` | `repo`, `kind` |
| `pg_hardstorage_repo_bytes` | `repo`, `kind` |

`kind` is one of `chunks`, `manifests`, `replicas`, `wal`,
`audit`, `tombstones`. Useful for billing breakdowns; see the
[cost report](cost-reporting.md) for the human view of the
same numbers.

### Verification + KMS

| Metric | Labels |
| --- | --- |
| `pg_hardstorage_verify_runs_total` | `deployment`, `result`, `tier` (`fast`/`full`/`sampled`) |
| `pg_hardstorage_kms_unwrap_latency_seconds` | (histogram, no labels) |

A KMS unwrap that exceeds the histogram's tail buckets is the
canonical "KMS unreachable or slow" symptom — pair with the
[KEK-unreachable alert](alerting-recipes.md#kek-unreachable).

### Anomaly + SLO

| Metric | Labels |
| --- | --- |
| `pg_hardstorage_anomaly_score` | `deployment`, `kind` (`size`/`churn`/`duration`) |
| `pg_hardstorage_rpo_seconds` | `deployment` |
| `pg_hardstorage_rto_estimate_seconds` | `deployment` |

The `anomaly_score` series is computed by the
[anomaly detector](../explanation/architecture-tour.md) — a
Z-score over the rolling 30-day distribution of each kind.
A score above 3 means the latest backup deviates by more than
three standard deviations from baseline.

### Control plane (live)

Served from the control plane's `/metrics`. The `route` label
is folded to a bounded set (`healthz`, `readyz`, `version`,
`deployments`, `agents`, `jobs`, `metrics`, `other`) so a job
ID or deployment name never mints a new series.

| Metric | Labels | Type |
| --- | --- | --- |
| `pg_hardstorage_http_requests_total` | `route`, `method`, `code` | counter |
| `pg_hardstorage_http_request_duration_seconds` | `route` | histogram |
| `pg_hardstorage_jobs` | `state` (`queued`/`running`/`completed`/`failed`/`cancelled`) | gauge |
| `pg_hardstorage_agents` | `state` (`active`/`total`) | gauge |
| `pg_hardstorage_repos_configured` | (none) | gauge |
| `pg_hardstorage_build_info` | `version`, `commit` (value always 1) | gauge |

### Agent liveness (reserved)

| Metric | Labels |
| --- | --- |
| `pg_hardstorage_agent_up` | `agent` |
| `pg_hardstorage_leader_election_state` | (gauge: 0=follower, 1=candidate, 2=leader) |

### Resilience namespace

The resilience metrics live in their own
`pg_hardstorage_resilience_*` sub-namespace so an SRE can
dashboard "did the system recover by itself?" separately from
the data-plane metrics:

| Metric | Labels |
| --- | --- |
| `pg_hardstorage_resilience_chunk_retries_total` | `deployment`, `reason` |
| `pg_hardstorage_resilience_manifest_repaired_total` | `source` (`replica`/`chunk-index`) |
| `pg_hardstorage_resilience_failover_handled_total` | `deployment`, `strategy` |
| `pg_hardstorage_resilience_slot_recreated_total` | `deployment`, `gap_bytes` |
| `pg_hardstorage_resilience_scrub_findings_total` | `kind` (`bit-rot`/`missing`/`orphan`) |
| `pg_hardstorage_resilience_panic_total` | `component` |
| `pg_hardstorage_resilience_backpressure_seconds_total` | `stage` |
| `pg_hardstorage_resilience_circuit_breaker_open_total` | `backend` |
| `pg_hardstorage_resilience_gameday_runs_total` | `scenario`, `result` |

### LLM telemetry

When the LLM helper is configured, every chat / skill run emits:

| Metric | Labels |
| --- | --- |
| `pg_hardstorage_llm_tokens_total` | `provider`, `model`, `direction` (`prompt`/`completion`) |

---

## OpenTelemetry

The agent has tracing wired in via
[`internal/obs/tracing`](https://github.com/cybertec-postgresql/pg_hardstorage/tree/main/internal/obs/tracing).
Default posture is **no-op**: code that calls
`tracing.Tracer().Start()` always works, and when nothing is
configured the spans are zero-overhead.

### Wiring an OTLP collector

Set the OTLP endpoint via config or env var:

```yaml
observability:
  tracing:
    otlp_endpoint: http://otel-collector.observability.svc:4318
    otlp_insecure: true       # plaintext localhost / sidecar
    sampler: parent_based_always_sample
```

Or:

```sh
PG_HARDSTORAGE_OTLP_ENDPOINT=http://otel-collector:4318 \
PG_HARDSTORAGE_OTLP_INSECURE=true \
pg_hardstorage agent
```

Air-gap mode refuses public collectors automatically — only
loopback and RFC1918 destinations are allowed.

### Span taxonomy

Top-level spans (one per operator-visible operation):

- `pg_hardstorage.backup`
- `pg_hardstorage.restore`
- `pg_hardstorage.wal.archive`
- `pg_hardstorage.verify`

High-value child spans:

- `pg.backup_start`
- `pg.basebackup.stream`
- `chunker.process_file`
- `storage.put_chunk` (with `dedup_hit` attribute)
- `kms.unwrap_dek`
- `pg.backup_stop`
- `manifest.commit`

We deliberately do not emit per-chunk spans — span overhead
on a 10000-chunk backup drowns out the interesting top-level
signal in any UI that doesn't aggregate. Per-chunk visibility
lives in metrics + sampled logs.

The instrumentation library name is
`github.com/cybertec-postgresql/pg_hardstorage` — filter on that to
isolate `pg_hardstorage` spans in a multi-tenant tracing
backend.

### Trace context propagation

W3C `traceparent` headers propagate agent ↔ control plane.
A scheduled backup initiated by the control plane carries the
parent span ID into the agent's `pg_hardstorage.backup` span,
so a single trace covers the full lifecycle.

---

## Structured logs

Logs go to stderr as one JSON object per line. The schema is
stable across versions:

```json
{
  "schema": "pg_hardstorage.log.v1",
  "timestamp": "2026-04-28T14:21:08.331Z",
  "level": "info",
  "component": "backup.orchestrator",
  "deployment": "db1",
  "backup_id": "db1.full.20260428T142108Z",
  "msg": "backup completed",
  "duration_ms": 28471,
  "logical_bytes": 13267243008,
  "physical_bytes": 4128376512,
  "dedup_ratio": 0.69
}
```

Severity floor follows RFC 5424 (`emergency`=0 …
`debug`=7). Lower the floor on a log sink with its
`min_severity` field in the config file (e.g.
`min_severity: debug` on the sink you forward logs to).

### Forwarding to a log sink

Logs duplicate to any configured sink whose severity floor
includes the message. The
[operator guide](operator-guide.md#10-sinks) covers sink
configuration. The relevant ones for log forwarding:

- `syslog` — RFC 5424 over UDP/TCP/TLS, octet-counted RFC 6587
  framing.
- `splunkhec` — direct to Splunk HEC.
- `datadog` — direct to Datadog Logs API.
- `otelevents` — emit as OpenTelemetry log events to the same
  collector running the trace exporter.

A sink that panics is recovered; sibling sinks still receive
the event; a diagnostic line lands on stderr.

### What does NOT go through structured logs

- **Audit events** — go through the
  [audit chain](operator-guide.md#8-audit-log) and the
  [evidence-bundle exporter](../compliance/audit-evidence-bundles.md).
  Logs are best-effort observability; the audit chain is
  forensic-grade.
- **Backup data** — never logs row-level data, even at debug.
  The PII redactor in
  [`internal/llm/privacy`](https://github.com/cybertec-postgresql/pg_hardstorage/tree/main/internal/llm/privacy)
  also covers log emission paths.

---

## Health endpoints

The agent's HTTP listener exposes:

| Endpoint | Purpose |
| --- | --- |
| `/healthz` | Liveness — the process is alive. Always 200 if reachable. |
| `/readyz` | Readiness — KMS reachable, repo reachable, leader-elected. |
| `/doctor` | Full doctor report as JSON; the same content as `pg_hardstorage doctor --json`. |
| `/metrics` | Prometheus scrape. Served by the control plane unconditionally; served by the agent only when started with `--metrics-listen`. |

Wire `/healthz` to your container liveness probe and `/readyz`
to the readiness probe. The
[Helm chart](../how-to/kubernetes/helm-server-chart.md) does
this for you.

---

## Further reading

- [Alerting recipes](alerting-recipes.md) — ready-to-paste
  PromQL rules.
- [Incident response](incident-response.md) — symptom →
  runbook mapping.
- [SLO as code](slo-as-code.md) — how RPO/RTO objectives wire
  into the alerting layer.
- [Capacity planning](capacity-planning.md) — projecting
  growth from these same metrics.
