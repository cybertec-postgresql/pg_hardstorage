---
title: Kubernetes how-to guides
description: Recipe-style pages for running pg_hardstorage on
              Kubernetes — Helm charts and operator integrations.
---

# Kubernetes how-to guides

Pages for running pg_hardstorage on Kubernetes. Three
shapes:

- **Helm charts** ship today for the host agent and reserve
  the chart name for the control plane.
- **Drop-in shims** (pgBackRest / Barman / WAL-G) ship in
  v1.1; they let an existing operator stack keep its
  pod / image / config shape and swap the backup binary
  underneath.
- **CNPG-I provider** for CloudNativePG is the long-form
  native integration on the v0.5 roadmap.

## Pages

### Helm charts

- [Deploy the sidecar Helm chart](helm-sidecar-chart.md) —
  the StatefulSet that runs the host agent for an external
  Postgres cluster.
- [Deploy the server Helm chart](helm-server-chart.md) —
  status and roadmap for the control-plane chart.

### Drop-in shims (v1.1)

- [Run as a pgBackRest shim (Crunchy PGO)](pgbackrest-shim.md)
  — drop-in replacement for `pgbackrest` inside Crunchy's
  `PostgresCluster`.
- [Run as a Barman shim (host-managed PG)](barman-shim.md)
  — drop-in replacement for `barman` /
  `barman-wal-archive` in pod-side wrappers.
- [Run as a WAL-G shim (Zalando)](walg-shim.md) — drop-in
  replacement for `wal-g` against the Zalando
  postgres-operator.

### Operator integrations (v0.5 roadmap)

- [Use the CloudNativePG-I provider](cnpg-i-provider.md) —
  CNPG `Cluster` plus our backup primitive underneath.

## Picking the right shape

| You run                                  | Use                                  |
|------------------------------------------|--------------------------------------|
| External PG (managed RDS / CloudSQL / VM) | [Sidecar chart](helm-sidecar-chart.md) |
| CloudNativePG (CNPG)                     | [CNPG-I provider](cnpg-i-provider.md) (v0.5)  |
| Zalando postgres-operator                | [WAL-G shim](walg-shim.md)            |
| Crunchy PGO                              | [pgBackRest shim](pgbackrest-shim.md) |
| Custom Barman pod (cron / Job)           | [Barman shim](barman-shim.md)         |

The sidecar chart pointed at the cluster's external endpoint
is always a viable alternative — it coexists with whatever
the operator is doing and gives you pg_hardstorage backups
in your own repo without touching the existing stack.
