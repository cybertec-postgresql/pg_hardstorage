---
title: Deploy the server Helm chart
description: Status and roadmap for the `pg-hardstorage-server`
              control-plane chart (v0.5).
tags:
  - kubernetes
  - helm
  - control-plane
  - roadmap
---

# Deploy the server Helm chart

> The `charts/pg-hardstorage-server` chart is a reserved
> placeholder for the v0.5 control-plane release. Until v0.5
> lands the chart renders no resources and installing it is
> a no-op. **For v0.1.x deployments, use
> [`pg-hardstorage-sidecar`](helm-sidecar-chart.md)**.

## What you need

- Awareness that this chart is a stub. Reading this page
  means you've found the placeholder and we want you to
  land on the right chart for today.

## Steps

### Today (v0.1.x): use the sidecar chart

```bash
helm install pg-hardstorage \
    ./charts/pg-hardstorage-sidecar \
    --namespace pg-hardstorage \
    --create-namespace \
    --values my-values.yaml
```

See [Deploy the sidecar Helm chart](helm-sidecar-chart.md)
for the full walkthrough.

### Once v0.5 lands

The server chart will host:

- The control-plane `pg_hardstorage server` binary —
  multi-host orchestration API + centralised retention
  engine.
- A REST `/v1/` surface (OpenAPI 3.1) for fleet operations.
- A gRPC streaming surface for high-volume agent traffic.
- A Postgres-backed coordination store (advisory locks +
  audit log). Recommends `postgresql` as a Helm dependency
  on install.
- The Generic CRDs (`pghardstorage.org/v1`: `HSDeployment`,
  `HSBackup`, `HSRestore`, `HSSchedule`).

The chart's `version` will follow the binary's; v0.5 ships
both at the same time so the chart references a real
`appVersion`.

## Why the stub exists

Helm chart names live in chart-museum / OCI registries
forever. Reserving `pg-hardstorage-server` now means:

- The v0.5 release ships under the right name with no
  rename / migration friction.
- Anyone browsing our repo today sees the intended split
  between **agent** (v0.1+) and **control plane** (v0.5+).
- Operators starting an evaluation today don't accidentally
  pin to a chart that's about to change.

## Roadmap

| Version | Adds                                                  |
|---------|-------------------------------------------------------|
| v0.5    | Server binary, REST API, retention engine, this chart |
| v0.6    | gRPC streaming, multi-tenant RBAC                     |
| v1.0    | GA, stable API contract, FIPS variant                 |

See `CHANGELOG.md` and `SPEC.md` for the binding versioning
policy.

## Next steps

- [Deploy the sidecar Helm chart](helm-sidecar-chart.md) —
  the supported v0.1 deployment shape.
- [Architecture tour](../../explanation/architecture-tour.md)
  — agent vs control plane.
