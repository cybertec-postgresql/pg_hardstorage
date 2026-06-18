---
title: Operations
description: Day-2 operator handbook.
---

# Operations

Day-2 operator content: how to keep `pg_hardstorage`
healthy, monitor it, plan capacity, and recover from
incidents.

## Pages

### Handbook

- [Operator guide](operator-guide.md) — installation,
  configuration, daily workflows.
- [Troubleshooting](troubleshooting.md) — common
  symptom-driven entry points.
- [Upgrade procedures](upgrade-procedures.md) — moving
  between minor / major releases without losing state.

### Observability

- [Monitoring](monitoring.md) — Prometheus scrape,
  OpenTelemetry tracing, structured-JSON logs, and the
  `pg_hardstorage_*` metric namespace.
- [Alerting recipes](alerting-recipes.md) — drop-in
  PromQL / Alertmanager rules for backup overdue, WAL
  silence, KEK unreachable, scrub findings, and more.

### Incident response

- [Incident response](incident-response.md) — symptom →
  action mapping with cross-links to the R1–R7 runbooks.

### Planning

- [Capacity planning](capacity-planning.md) —
  `pg_hardstorage capacity report` with 30/90/365-day
  projection.
- [Cost reporting](cost-reporting.md) — per-deployment
  and per-tenant repo-cost exports.
- [SLO as code](slo-as-code.md) — declarative RPO/RTO
  targets and the alert wiring.
- [Scaling to large fleets](scaling-large-fleets.md) — staying
  fast and bounded across thousands of deployments: the
  job-concurrency cap, agent poll jitter, sharded audit chains,
  and the deployment index.
