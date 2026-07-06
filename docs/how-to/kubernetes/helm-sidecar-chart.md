---
title: Deploy the sidecar Helm chart
description: Run the host agent as a Kubernetes-native StatefulSet
              for an external Postgres cluster.
tags:
  - kubernetes
  - helm
  - sidecar
  - agent
---

# Deploy the sidecar Helm chart

> Install `charts/pg-hardstorage-sidecar` to run the host
> agent as a StatefulSet that backs up an external
> self-managed PostgreSQL cluster (bare metal or
> VMs reachable from the cluster network). One replica;
> persistent state PVC; the agent's same posture you'd run
> as a `systemd` service on a VM, packaged for K8s.

## What you need

- A Kubernetes cluster, version ≥ 1.24 (chart's
  `kubeVersion` floor).
- `helm` 3.x.
- A reachable PostgreSQL endpoint from the cluster network.
- A KMS endpoint or local keyring for the encryption KEK.
- A repository URL the agent can write to (S3 / GCS / Azure
  Blob / SFTP / NFS-backed PVC).

## Steps

### 1. Add the chart locally

For v0.1, install from the in-tree chart:

```bash
helm install pg-hardstorage \
    ./charts/pg-hardstorage-sidecar \
    --namespace pg-hardstorage \
    --create-namespace \
    --values my-values.yaml
```

The in-tree chart is the only supported install path today.
No OCI Helm chart is published yet;
`oci://ghcr.io/cybertec-postgresql/pg-hardstorage-sidecar`
does not exist. Once it is published, the install will
collapse to a single-line `helm install pg-hardstorage
oci://ghcr.io/cybertec-postgresql/pg-hardstorage-sidecar`.

### 2. Provide a values file

The default `values.yaml` ships an empty `config:` so a
no-overrides install fails fast. Minimum viable config:

```yaml
# my-values.yaml
config: |
  deployments:
    db1:
      pg_connection: postgres://pgbackup@db1.example.com/postgres
      repo: s3://acme-pg-backups
      retention:
        policy: gfs
        keep_daily: 7
        keep_weekly: 4
        keep_monthly: 12
        keep_yearly: 5
      schedule:
        full:        "0 2 * * 0"   # Sundays 02:00
        incremental: "0 2 * * 1-6" # Mon-Sat 02:00

env:
  - name: PG_HARDSTORAGE_KEYRING_PASSPHRASE
    valueFrom:
      secretKeyRef:
        name: pg-hardstorage-keyring
        key: passphrase

persistence:
  enabled: true
  size: 50Gi
  storageClass: gp3
```

The `config:` block is rendered into a `ConfigMap` and
mounted at `/etc/pg_hardstorage/pg_hardstorage.yaml`. A
checksum annotation rolls the pod when the ConfigMap
changes — `helm upgrade` of values actually takes effect.

### 3. Apply

```bash
helm install pg-hardstorage ./charts/pg-hardstorage-sidecar \
    -n pg-hardstorage --create-namespace \
    -f my-values.yaml
```

### 4. Verify the agent is up

```bash
kubectl -n pg-hardstorage get sts,pvc,svc
kubectl -n pg-hardstorage logs sts/pg-hardstorage -f
kubectl -n pg-hardstorage exec sts/pg-hardstorage-0 -- pg_hardstorage doctor
```

The Service exposes `:9090/metrics` for Prometheus scraping
and `:9090/healthz` (lands with v0.2; the chart points the
v0.1 probes at `/metrics`, which the agent serves once up).

## What just happened

The chart deployed:

- A **StatefulSet** with exactly one replica. The agent's
  local state (inflight markers, audit log, manifest
  cache) lives in a PVC and must stay attached to a stable
  pod identity. A `Deployment` would lose pod ordinality
  on rollout.
- A **ConfigMap** holding `pg_hardstorage.yaml`, mounted
  read-only at `/etc/pg_hardstorage/`.
- A **Service** (ClusterIP by default) on port 9090.
- A **ServiceAccount** (token mount disabled — the v0.1
  agent doesn't talk to the K8s API).
- A **PVC** for `/var/lib/pg_hardstorage` (default 10 GiB).

Pod security context is `runAsNonRoot: true`,
`runAsUser: 65532` (the distroless `:nonroot` UID),
`readOnlyRootFilesystem: true`, with `/tmp` mounted as a
64 MiB tmpfs `emptyDir`. All capabilities dropped.

## Why a single replica

The v0.1 agent coordinates state via filesystem locks; it
doesn't yet support active/active. Setting `replicaCount`
above 1 produces duplicate backups and audit-log churn —
the chart accepts the value but does not protect you from
the consequence. Active/active lands with the v0.5 control
plane.

## Configuring the KMS / keyring

Two patterns:

### Inline keyring (small deployments)

The keyring directory `/etc/pg_hardstorage/keyring/` mounts
as part of the ConfigMap. Useful for plaintext-only setups
or for a passphrase wrapper backed by a sealed Secret.

### KMS provider (recommended)

Point `kms:` at AWS / GCP / Azure / Vault / PKCS#11 in the
config block. The agent's IAM (via IRSA / Workload Identity
/ Pod Identity) handles auth. No long-lived secrets in the
cluster.

Example:

```yaml
config: |
  kms:
    default:
      type: aws-kms
      region: eu-central-1
      key_id: arn:aws:kms:eu-central-1:111122223333:key/abcd-...
```

For the AWS path the chart's ServiceAccount carries the
IRSA annotation:

```yaml
serviceAccount:
  annotations:
    eks.amazonaws.com/role-arn: arn:aws:iam::111122223333:role/pg-hardstorage
```

## Probes

| Probe | Endpoint | Notes |
| --- | --- | --- |
| `livenessProbe` | `/metrics` | v0.2 will move this to `/healthz`. |
| `readinessProbe` | `/metrics` | Same. |

`/healthz` checks process liveness; `/readyz` will check
KMS reachability + repo reachability + leader-elected once
the control-plane lands. v0.1's `/metrics` smoke is
"process is up and serving HTTP", which is enough for the
sidecar's single-replica posture.

## Troubleshooting

### Pod stuck `CreateContainerConfigError`

Almost always a missing Secret referenced via `env:`.
`kubectl describe pod` lists the missing key. Create the
Secret, the StatefulSet picks it up on next pod create.

### `pg_hardstorage doctor` fails inside the pod

Same triage as a VM install: the doctor section that fails
tells you the specific cause (KMS unreachable, repo not
writable, replication slot missing, etc.). Each finding
prints `Suggested fix:` with the exact command.

### PVC bound but state empty across pod restarts

The pod template's `state` volumeClaim is per-replica; if
you've scaled past 1, replica 1's PVC is fresh. Don't
scale past 1 (see [Why a single replica](#why-a-single-replica)).

### Chart upgrade doesn't pick up new config

The pod rolls on `checksum/config` annotation change, but
not on changes to `env:` or `args:` alone. Force a roll:

```bash
kubectl -n pg-hardstorage rollout restart sts/pg-hardstorage
```

## Service-mesh / mTLS

The chart doesn't impose a mesh. If you run Istio / Linkerd
/ Cilium, the agent's outbound calls go through whatever
sidecar your mesh injects. The agent itself doesn't open a
connection to the K8s API at v0.1 (`rbac.create: false`),
so no mesh-side authorization rules need adjusting.

## Next steps

- [Deploy the server chart](helm-server-chart.md) — what
  the `pg-hardstorage-server` chart will host once v0.5
  ships.
- [Operator integration via CNPG-I](cnpg-i-provider.md) —
  the per-cluster integration pattern for CloudNativePG.
- [WAL-G shim](walg-shim.md) — the Zalando integration
  pattern.
- [pgBackRest shim](pgbackrest-shim.md) — the Crunchy PGO
  integration pattern.
