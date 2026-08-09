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
        backup:
          daily_at: "02:00"
        rotate:
          daily_at: "04:00"

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

### Local keyring (`kek_ref: local:...`)

The keyring directory `/etc/pg_hardstorage/keyring/` mounts as an
in-memory volume populated from a **Secret** — never the ConfigMap. Key material in a
ConfigMap is readable by anyone with `get configmap`, which
is a lower bar than `get secret` in any cluster that
separates the two.

The Secret is not mounted at the keyring path directly. Kubernetes
rewrites the modes of Secret-mounted files when the pod runs with an
`fsGroup` (group-read gets OR'd in — `defaultMode: 0600` arrives as
`0640`), and the agent's keystore refuses any group- or
world-readable `kek.bin` or signing key: that refusal is a security
property, not a bug. The chart therefore runs an `install-keyring`
initContainer — the agent's own `pg_hardstorage keyring install` —
which copies the mounted material into an in-memory volume with
owner-only modes. You will see this as a short-lived init step on
pod start; if your Secrets are misnamed or missing, it is this step
that fails, loudly, with the offending path in its output.

Bring your own Secret (recommended). Works with
sealed-secrets, external-secrets and CSI drivers, and keeps
the key out of your values file and out of Helm history:

```bash
kubectl create secret generic pgh-keyring \
    --from-file=kek.bin=/path/to/kek.bin
```

```yaml
keyring:
  existingSecret: pgh-keyring
```

Or inline it for a small deployment. Values are base64
because `kek.bin` is 32 raw bytes and cannot be carried as
YAML text — and anything here lands in your values file and
in `helm get values`:

```yaml
keyring:
  files:
    kek.bin: "3q2+7w=="   # base64 of the raw key
```

Neither is rendered when both are empty, so a KMS-backed
deployment mounts nothing extra.

The keyring holds three files, and they must share one
directory:

| File | Purpose |
| --- | --- |
| `kek.bin` | the local KEK |
| `manifest_signing.ed25519` | manifest signing key (private) |
| `manifest_signing.pub` | its public half |

When those come from **different** Secrets — the usual
shape once the signing key is managed by sealed-secrets or
external-secrets while the KEK comes from elsewhere — name
them individually. The chart then mounts a projected volume,
which merges several sources into one directory; three
separate `secret` volumes cannot share a mount path.

```yaml
keyring:
  kek:
    secretName: pgh-kek
    key: kek.bin                    # key WITHIN that Secret
  signingKey:
    secretName: pgh-signing
    key: private.pem
  signingPub:
    secretName: pgh-signing
    key: public.pem
```

`key` is how *you* keyed it; the filename the agent opens is
fixed by the product and is not configurable. A keyring with
the right bytes under the wrong name looks fully populated
and does not work — `doctor` reports the signing key absent
and nothing says the cause was a filename — so the chart
writes the correct names regardless of your keying, and a
test pins them against the product's own constants.

Any subset works: name only `kek` and the other two simply
are not mounted.

Anything the chart does not model — a CSI-mounted key, a CA
bundle — goes through `extraVolumes` and `extraVolumeMounts`,
which are applied verbatim.

### KMS provider (recommended)

Point `kms:` at AWS / GCP / Azure / Vault / PKCS#11 in the
config block. The agent's IAM (via IRSA / Workload Identity
/ Pod Identity) handles auth. No long-lived secrets in the
cluster.

Example:

```yaml
config: |
  kms:
    providers:
      - kek_ref: aws-kms://arn:aws:kms:eu-central-1:111122223333:key/abcd-...
        config:
          region: eu-central-1
  deployments:
    db1:
      repo: s3://acme-pg-backups/
      kek_ref: aws-kms://arn:aws:kms:eu-central-1:111122223333:key/abcd-...
```

The deployment's `kek_ref` is what the sidecar's scheduled
backups and its WAL archiver resolve against — no `--kek`
flag is involved, because neither has a command line.

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
