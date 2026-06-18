# pg-hardstorage-server (stub)

This chart is a placeholder. The pg_hardstorage **control-plane
server** is scheduled to land with v0.5 alongside the multi-host
orchestration API and the centralised retention engine. Until
then this chart renders no resources.

## What to use today

For v0.1.x, deploy [`pg-hardstorage-sidecar`](../pg-hardstorage-sidecar/)
instead. It packages the host agent — the same binary you would
run as a `systemd` service on a VM — as a Kubernetes-native
StatefulSet. That is the supported v0.1 deployment shape on
Kubernetes.

## Why the stub exists

Helm chart names live in chart-museum / OCI registries forever.
We reserve `pg-hardstorage-server` now so the v0.5 release does
not need a renamed chart and so anyone browsing our repo sees the
intended split between **agent** and **control plane**.

## Roadmap

| Version | Adds                                                  |
|---------|-------------------------------------------------------|
| v0.5    | Server binary, REST API, retention engine, this chart |
| v0.6    | gRPC streaming, multi-tenant RBAC                     |
| v1.0    | GA, stable API contract, FIPS variant                 |

See `CHANGELOG.md` and `docs/SPEC.md` in the repository root for
the binding versioning policy.
