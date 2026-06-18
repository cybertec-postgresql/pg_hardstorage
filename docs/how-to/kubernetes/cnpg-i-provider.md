---
title: Use the CloudNativePG-I provider
description: Plug `pg_hardstorage` into a CloudNativePG cluster as
              a CNPG-I backup provider.
tags:
  - kubernetes
  - cnpg
  - cloudnativepg
  - integration
  - roadmap
---

# Use the CloudNativePG-I provider

> Wire `pg_hardstorage` into a CloudNativePG (CNPG) cluster
> as a CNPG-I backup provider. CNPG calls into our binary
> for backup / WAL archive / restore; the cluster's
> `Cluster` CR becomes the integration surface and no
> Cluster manifest cares which backup tool is underneath.

!!! note "Roadmap"
    The CNPG-I provider lands with **v0.5**. The agent and
    bundle primitives in v0.1 already speak CNPG-shaped
    backups; the bridge code that exposes them as a CNPG-I
    provider is what's deferred. This page documents the
    **target shape** so v0.1 operators can plan for the
    upgrade. For today, use the
    [sidecar chart](helm-sidecar-chart.md) directly.

## What you need (once v0.5 lands)

- A CloudNativePG installation, version compatible with the
  CNPG-I provider contract pinned by `pg_hardstorage v0.5`.
- The `pg-hardstorage-server` chart (the v0.5 control
  plane). See [helm-server-chart](helm-server-chart.md).
- A repository URL accessible from the cluster network.

## Steps (target shape)

### 1. Install the control plane

```bash
helm install pg-hardstorage \
    oci://ghcr.io/cybertec-postgresql/pg-hardstorage-server \
    --namespace pg-hardstorage \
    --create-namespace
```

### 2. Reference it from a CNPG `Cluster`

```yaml
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: db1
spec:
  instances: 3
  storage:
    size: 100Gi
  backup:
    pluginConfiguration:
      name: pg-hardstorage.cybertec.at
      parameters:
        repo: s3://acme-pg-backups
        retention: gfs
        kek_ref: aws-kms://arn:aws:kms:eu-central-1:111122223333:key/abcd-...
```

The `pluginConfiguration.name` is the CNPG-I plugin
identifier our control plane registers; CNPG calls into
our binary for every backup / WAL archive / restore the
cluster needs.

### 3. (Optional) Use a `HSDeployment` CRD

The control plane also ships generic CRDs
(`pghardstorage.org/v1`) for operators who want to drive
backups without going through CNPG's `Cluster` spec:

```yaml
apiVersion: pghardstorage.org/v1
kind: HSDeployment
metadata:
  name: db1
spec:
  pgConnection: postgres://pgbackup@db1.example.com/postgres
  repo: s3://acme-pg-backups
  schedule:
    full: "0 2 * * 0"
    incremental: "0 2 * * 1-6"
```

`HSBackup`, `HSRestore`, `HSSchedule` complete the surface.

### API-group rename note (issue #83)

The CRD API group was renamed from `pg-hardstorage.io/v1` to
`pghardstorage.org/v1` to align with the canonical project
domain.  The rename ships as a **clean** change rather than
with a dual-group back-compat window: the CRDs are part of the
v0.5+ surface that has not had a stable public release yet, so
no deployed cluster carries CRDs under the old group.  If you
have been tracking the placeholder docs and built `HSDeployment`
objects locally under `pg-hardstorage.io/v1`, re-apply them under
the new group — the spec field shapes are unchanged.

## What's coming under the hood (target shape)

The CNPG-I provider is a thin gRPC adapter:

- CNPG → `pg-hardstorage-server` over gRPC for `Backup`,
  `WALArchive`, `Restore` operations.
- Server fans out to the appropriate agent (which is
  pinned to the CNPG cluster's primary via Patroni REST
  awareness — the same mechanism the v0.1 agent already
  has, just lit up by the operator integration).
- Result events flow back to CNPG; the cluster's status
  reflects backup state in CNPG's native CRs.

The CNPG `Cluster` operator stays the contract surface for
the cluster team; the `pg_hardstorage` server / agent stack
is an implementation detail underneath.

## Why CNPG-I rather than a parallel CR

CNPG users already manage their clusters through
`Cluster`. Adding a parallel `HSDeployment` for every CNPG
cluster would mean two sources of truth for "which Postgres
exists where," with all the drift that implies. CNPG-I lets
us plug into the existing surface without forcing an
operational pivot.

For non-CNPG users, the generic CRDs (`HSDeployment` and
friends) are the right entry point — they don't depend on
CNPG being installed.

## What works today (v0.1 path)

Today, the agent runs as a StatefulSet via the
[sidecar chart](helm-sidecar-chart.md), pointed at the
external endpoint of the CNPG cluster's primary. The
backups it produces are byte-identical to what the v0.5
CNPG-I provider will produce — only the integration
surface changes. State migration from v0.1 to v0.5 is "no
action": same repo, same manifests, same chunks.

## Roadmap

| Version | Adds                                                  |
|---------|-------------------------------------------------------|
| v0.1    | Agent (StatefulSet via sidecar chart), Patroni REST awareness |
| v0.5    | Control plane, CNPG-I provider, generic CRDs          |
| v0.6    | gRPC streaming for high-volume CNPG fleets            |
| v1.0    | GA, stable plugin contract                            |

## Next steps

- [Deploy the sidecar Helm chart](helm-sidecar-chart.md) —
  the v0.1 path.
- [Deploy the server Helm chart](helm-server-chart.md) —
  the v0.5 control plane.
- [WAL-G shim](walg-shim.md) — the Zalando-operator
  integration pattern.
- [pgBackRest shim](pgbackrest-shim.md) — the Crunchy PGO
  integration pattern.
