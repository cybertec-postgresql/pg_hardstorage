---
title: Run as a WAL-G shim (Zalando)
description: Drop-in replacement for `wal-g` against the Zalando
              postgres-operator's WALG_* env vars.
tags:
  - kubernetes
  - zalando
  - walg
  - integration
---

# Run as a WAL-G shim (Zalando)

> Use the **`pg-hardstorage-walg`** binary as a drop-in for
> the WAL-G CLI invocations the Zalando postgres-operator
> emits. The Zalando cluster spec stays identical;
> pg_hardstorage handles the actual backup, WAL archive,
> WAL prefetch, restore.

The shim ships in pg_hardstorage v1.1 and follows the
[`compat/walg` architecture](https://github.com/cybertec-postgresql/pg_hardstorage/tree/main/compat/walg):
the binary reads the same `WALG_*` env vars + the same five
CLI verbs (`backup-push`, `backup-fetch`, `backup-list`,
`wal-push`, `wal-fetch`), builds a synthetic argv for the
native CLI, and dispatches via the public command surface.

## What this is

The Zalando postgres-operator drives backups by exec'ing
the `wal-g` binary inside the Spilo container with a
defined set of env vars (`WALG_S3_PREFIX`, `AWS_*`,
`WALG_LIBSODIUM_KEY`, …) and CLI verbs.  The shim:

1. Parses the same env vars + CLI verbs WAL-G accepts.
2. Translates the request into the equivalent
   pg_hardstorage operation against the configured repo +
   KMS.
3. Returns exit codes in the shape WAL-G produces, so the
   operator's parsing logic doesn't need to change.

The result: a Zalando cluster running with our shim looks
identical to one running with WAL-G, except the bytes on
the repo are pg_hardstorage-flavour (content-addressed
chunks, signed manifests, sandbox-verifiable).

## What you need

- A Zalando postgres-operator installation.
- A custom Spilo image that has `pg-hardstorage-walg`
  shadowing `wal-g` on PATH.
- A repository URL accessible from the cluster pods.

## Steps

### 1. Build a Spilo image with the shim

```dockerfile
FROM golang:1.26 AS shim-builder
WORKDIR /src
# Clone or COPY the pg_hardstorage source here.
RUN git clone https://github.com/cybertec-postgresql/pg_hardstorage . \
 && CGO_ENABLED=0 go build -o /out/pg-hardstorage-walg ./cmd/pg-hardstorage-walg

FROM ghcr.io/zalando/spilo-15:latest

COPY --from=shim-builder /out/pg-hardstorage-walg /usr/bin/pg-hardstorage-walg

# Replace wal-g with the shim wrapper.
RUN ln -sf /usr/bin/pg-hardstorage-walg /usr/bin/wal-g

# Some Spilo configurations consult WAL_E_BIN / WAL_G_BIN env
# vars in the postgres-operator's pod template — point them at
# the shim if you'd rather not symlink.
```

The compat shims are **not** published in any image: the
`ghcr.io/cybertec-postgresql/pg_hardstorage` image is
distroless and carries only the `pg_hardstorage` binary. You
build `pg-hardstorage-walg` from its `cmd/pg-hardstorage-walg`
package (a single static Go binary, <30 MiB) as shown in the
builder stage above.

### 2. Configure via standard Zalando env vars

The operator already plumbs `WALG_S3_PREFIX`, `AWS_*`,
`WALG_COMPRESSION_METHOD` etc.  The shim consumes the same
set; what gets translated:

| WAL-G env var            | pg_hardstorage equivalent       |
|--------------------------|---------------------------------|
| `WALG_S3_PREFIX`         | `--repo s3://…`                 |
| `WALG_GS_PREFIX`         | `--repo gs://…`                 |
| `WALG_AZURE_PREFIX`      | `--repo azure://…`              |
| `WALG_FILE_PREFIX`       | `--repo file:///…`              |
| `WALG_SSH_PREFIX`        | `--repo sftp://…` (mapped)      |
| `AWS_ACCESS_KEY_ID` etc. | picked up by the AWS SDK chain  |
| `PGHOST` / `PGPORT` / `PGUSER` / `PGDATABASE` | `--pg-connection postgres://…` |
| `WALG_COMPRESSION_METHOD`| warning if not zstd; native uses zstd by default |
| `WALG_DELTA_MAX_STEPS`   | implicit (incrementals roll back automatically) |
| `WALG_LIBSODIUM_KEY` / `WALG_GPG_KEY_ID` / `WALG_PGP_KEY[_PATH]` | **refused** with a pointer to `encryption.kek_ref` (the algorithms are not byte-compatible) |

`PG_HARDSTORAGE_DEPLOYMENT` (a pg_hardstorage-specific
opt-in) sets the deployment name explicitly; otherwise the
shim defaults to the bare `PGHOST` value, or `default` if
neither is set.

The full mapping is in
[`compat/walg/flags.go`](https://github.com/cybertec-postgresql/pg_hardstorage/blob/main/compat/walg/flags.go);
unmapped vars produce a `pg-hardstorage-walg: warn:` line
on stderr at runtime.

### 3. Update the cluster spec to use the new image

```yaml
apiVersion: acid.zalan.do/v1
kind: postgresql
metadata:
  name: db1
spec:
  dockerImage: registry.example.com/spilo-pg-hardstorage:15-v1.1
  numberOfInstances: 3
  postgresql:
    version: "15"
  volume:
    size: 100Gi
```

### 4. Confirm backups are landing in the new shape

```bash
pg_hardstorage list db1 --repo s3://acme-pg-backups
```

If the manifests look like `db1.full.<timestamp>` and the
repo layout matches our standard `manifests/<deployment>/…`
structure, you're running on the shim.

## Refusal contract

11 less-common WAL-G verbs (`delete`, `backup-mark`,
`catchup-push`, `catchup-fetch`, `wal-receive`,
`wal-verify`, `wal-show`, `st`, `copy`, `daemon`,
`backup-show`) and `--permanent` exit with code 2 and a
one-line message of the form

```
pg-hardstorage-walg: <command>: not implemented in v1.1;
native equivalent: <suggestion>
```

The Zalando operator surfaces the message; the operator-
facing error is identical to a real WAL-G non-zero exit, so
existing alerting works unchanged.

## What's NOT in the shim

- **Reading existing WAL-G repos.**  Old repos remain
  readable by `wal-g` only.  The
  [migration playbook](../migration/from-walg.md) covers
  dual-write + retention drain.
- **Encryption envelope re-keying.**  WAL-G's libsodium /
  GPG / PGP envelopes are not byte-compatible with native
  AES-256-GCM.  The shim refuses if those env vars are set;
  configure `encryption.kek_ref` in pg_hardstorage.yaml
  before activating.

## Why a shim, not a fork

Forking the postgres-operator fragments the ecosystem.
Shimming WAL-G means:

- Every existing Zalando deployment runbook still works.
- Backup / restore primitives upgrade in place; no cluster
  spec changes (modulo image swap).
- The operator team owns the cluster lifecycle; we own the
  backup primitive.

## Alternative: native sidecar

If you'd rather not modify the Spilo image, run the
[sidecar chart](helm-sidecar-chart.md) against the Zalando
cluster's external endpoint.  It coexists with WAL-G's
existing in-pod backups; you get pg_hardstorage backups in
your own repo without touching the operator.

## Next steps

- [Migrate from WAL-G](../migration/from-walg.md) — for
  operators moving off vanilla WAL-G entirely (the v1.1
  fast path is built around this same shim).
- [pgBackRest shim](pgbackrest-shim.md) — same drop-in
  pattern for Crunchy PGO.
- [Barman shim](barman-shim.md) — same drop-in pattern for
  Barman in host-managed deployments.
- [Sidecar chart](helm-sidecar-chart.md) — the no-fork
  alternative.
