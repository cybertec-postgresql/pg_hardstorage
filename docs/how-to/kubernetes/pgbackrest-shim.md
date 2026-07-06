---
title: Run as a pgBackRest shim (Crunchy PGO)
description: Drop-in replacement for the `pgbackrest` binary
              inside a Crunchy PGO `PostgresCluster`.
tags:
  - kubernetes
  - crunchy
  - pgo
  - pgbackrest
  - integration
---

# Run as a pgBackRest shim (Crunchy PGO)

> Use the **`pg-hardstorage-pgbackrest`** binary as a drop-in
> replacement for the `pgbackrest` binary that Crunchy's
> `PostgresCluster` operator (PGO) exec's. The operator's
> CR shape is unchanged; pg_hardstorage handles the backup /
> archive / restore plumbing underneath.

The shim ships in pg_hardstorage v1.1 and follows the
[`compat/pgbackrest` architecture](https://github.com/cybertec-postgresql/pg_hardstorage/tree/main/compat/pgbackrest):
the binary parses pgBackRest flags + config, builds a
synthetic argv for the native CLI, and dispatches via the
public command surface (`pg_hardstorage backup`,
`pg_hardstorage restore`, …).  No internal-API coupling, no
fork — one process, one binary on disk.

## What this is

Crunchy PGO drives backups by exec'ing the `pgbackrest`
binary inside the postgres pod with a defined set of
config flags + verbs (`stanza-create`, `backup`,
`archive-push`, `archive-get`, `restore`, `info`, `check`,
`verify`).  The shim:

1. Parses the pgBackRest config + CLI invocations PGO emits.
2. Translates each call into the equivalent pg_hardstorage
   command against the configured repo + KMS.
3. Emits stdout / exit codes in the shape pgBackRest
   produces, so PGO's parsing logic doesn't need to change.

The result: a `PostgresCluster` running with our shim looks
identical to one running with pgBackRest, except the bytes
on the repo are pg_hardstorage-flavour (content-addressed,
signed, sandbox-verifiable).

## What you need

- A Crunchy PGO installation.
- A custom container image with `pg-hardstorage-pgbackrest`
  shadowing `pgbackrest` on PATH.
- A repository URL accessible from the cluster pods.

## Steps

### 1. Build a PGO image with the shim

```dockerfile
FROM golang:1.26 AS shim-builder
WORKDIR /src
# Clone or COPY the pg_hardstorage source here.
RUN git clone https://github.com/cybertec-postgresql/pg_hardstorage . \
 && CGO_ENABLED=0 go build -o /out/pg-hardstorage-pgbackrest ./cmd/pg-hardstorage-pgbackrest

FROM registry.developers.crunchydata.com/crunchydata/crunchy-postgres:ubi9-15

COPY --from=shim-builder /out/pg-hardstorage-pgbackrest /usr/bin/pg-hardstorage-pgbackrest

# Drop-in: pgbackrest invocations land at our binary.
RUN ln -sf /usr/bin/pg-hardstorage-pgbackrest /usr/bin/pgbackrest
```

The compat shims are **not** published in any image: the
`ghcr.io/cybertec-postgresql/pg_hardstorage` image is
distroless and carries only the `pg_hardstorage` binary. You
build each shim from its `cmd/pg-hardstorage-*` package (a
single static Go binary, <30 MiB) as shown in the builder
stage above.

### 2. Configure via standard PGO repo spec

PGO already plumbs `pgBackRestConfig` and the
`backups.pgbackrest` block of the `PostgresCluster` spec.
The shim consumes the same set; what gets translated:

| pgBackRest config       | pg_hardstorage equivalent      |
|-------------------------|--------------------------------|
| `repo1-path`            | `--repo` URL prefix            |
| `repo1-s3-bucket`       | `s3://<bucket>/…`              |
| `repo1-cipher-pass`     | KMS envelope (passphrase-derived KEK) |
| `repo1-cipher-type`     | algorithm note (CBC → GCM)      |
| `compress-type`         | `compression:` config (zstd default) |
| `process-max`           | `parallelism:` config          |
| `--stanza`              | deployment positional argument |

The full mapping is in
[`compat/pgbackrest/flags.go`](https://github.com/cybertec-postgresql/pg_hardstorage/blob/main/compat/pgbackrest/flags.go);
unmapped settings produce a `pg-hardstorage-pgbackrest:
warn:` line on stderr at runtime.

### 3. Use the modified image in `PostgresCluster`

```yaml
apiVersion: postgres-operator.crunchydata.com/v1beta1
kind: PostgresCluster
metadata:
  name: db1
spec:
  postgresVersion: 15
  image: registry.example.com/crunchy-pg-hardstorage:ubi9-15-v1.1
  instances:
    - name: instance1
      replicas: 3
      dataVolumeClaimSpec:
        storageClassName: gp3
        accessModes: ["ReadWriteOnce"]
        resources:
          requests:
            storage: 100Gi
  backups:
    pgbackrest:
      image: registry.example.com/crunchy-pgbackrest-shim:v1.1
      repos:
        - name: repo1
          s3:
            bucket: acme-pg-backups
            endpoint: s3.amazonaws.com
            region: eu-central-1
```

### 4. Confirm backups are landing in pg_hardstorage shape

```bash
pg_hardstorage list db1 --repo s3://acme-pg-backups
```

Manifests with `db1.full.<timestamp>` IDs and our standard
repo layout = the shim is active.

## Refusal contract

Verbs not in the v1.1 surface, and flags like `--type=diff`,
exit with code 2 and a one-line message of the form

```
pg-hardstorage-pgbackrest: <command>: not implemented in v1.1;
native equivalent: <suggestion>
```

PGO retries the operation and surfaces the message; the
operator-facing error is identical to a real pgBackRest
non-zero exit, so existing alerting works unchanged.

## What's NOT in the shim

- **Reading existing pgBackRest repos.**  Old repos remain
  readable by `pgbackrest` only.  The
  [migration playbook](../migration/from-pgbackrest.md)
  covers dual-write + retention drain.
- **Byte-identical output.**  Semantic equivalence is the
  contract — `info` returns pg_hardstorage's deployment
  view, not pgBackRest's stanza listing.

## Why a shim, not a fork

- Crunchy PGO owns the cluster lifecycle, resource shape,
  upgrade flow.
- We own the backup primitive: incremental, content-
  addressed, sandbox-verified, optionally KMS-wrapped.
- A shim lets PGO users keep their runbooks and gives them
  our backup posture without operator-team buy-in.

## Alternative: native sidecar

If you'd rather not modify the PGO image, run the
[sidecar chart](helm-sidecar-chart.md) against the
`PostgresCluster`'s external endpoint.  It coexists with
PGO's existing pgBackRest backups; you get pg_hardstorage
backups in your own repo without touching the operator.

## Next steps

- [Migrate from pgBackRest](../migration/from-pgbackrest.md)
  — for operators moving off pgBackRest entirely (the v1.1
  fast path is built around this same shim).
- [Barman shim (host-managed PG)](barman-shim.md) — same
  drop-in pattern for Barman.
- [WAL-G shim (Zalando)](walg-shim.md) — same drop-in
  pattern for WAL-G inside a Zalando-operated cluster.
- [Sidecar chart](helm-sidecar-chart.md) — the no-fork
  alternative.
