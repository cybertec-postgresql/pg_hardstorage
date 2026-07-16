# pg-hardstorage-sidecar Helm chart

Deploys the `pg_hardstorage` host agent into Kubernetes as a
single-replica **StatefulSet** that backs up an *external*,
self-managed PostgreSQL cluster reachable over the cluster network —
bare metal or VMs. Use it when Postgres lives outside Kubernetes but
you want backups, WAL streaming, and PITR driven from a
Kubernetes-native workload. (Fully-managed DBaaS such as RDS / Cloud
SQL are **not supported** — they do not expose `BASE_BACKUP` /
physical replication to customers.)

> The "sidecar" name is historic: the v0.1 chart packages a
> standalone agent. True per-pod sidecar injection lands with the
> v0.5 operator.

## Install

```sh
helm install pg-hardstorage ./charts/pg-hardstorage-sidecar \
  --values my-values.yaml
```

A bare `helm install` with no overrides fails fast on purpose:
`config` defaults to empty, so you must supply the
`pg_hardstorage.yaml` the agent reads.

## Key values

| Value          | Default | Notes |
| -------------- | ------- | ----- |
| `image.repository` | `ghcr.io/cybertec-postgresql/pg_hardstorage` | Pin to a digest in production. |
| `image.tag`    | `""` (tracks `appVersion`) | |
| `replicaCount` | `1` | Always 1 in v0.1 — the agent coordinates state via filesystem locks; higher values produce duplicate backups. |
| `config`       | `""` (required) | The `pg_hardstorage.yaml` body, rendered into a ConfigMap and mounted at `/etc/pg_hardstorage/pg_hardstorage.yaml`. |
| `env`          | `[]` | Extra env vars — commonly a keyring passphrase via `secretKeyRef`. |

See [`values.yaml`](values.yaml) for the full, commented set.

## Templates

- [`statefulset.yaml`](templates/statefulset.yaml) — the agent workload + state PVC
- [`configmap.yaml`](templates/configmap.yaml) — renders `config` into the agent's config file
- [`service.yaml`](templates/service.yaml) — headless service for stable pod identity
- [`serviceaccount.yaml`](templates/serviceaccount.yaml) — the agent's identity
- [`NOTES.txt`](templates/NOTES.txt) — post-install guidance

## Related docs

- [Kubernetes how-to guides](../../docs/how-to/kubernetes/index.md)
- [Kubernetes (CNPG) tutorial](../../docs/tutorials/kubernetes-cnpg.md)
- [Helm sidecar-chart how-to](../../docs/how-to/kubernetes/helm-sidecar-chart.md)
