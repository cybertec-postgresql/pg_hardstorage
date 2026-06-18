---
title: Verify a backup with the Docker sandbox
description: Run `verify --full` against a postgres:<major> container — the
              default verifier sandbox.
tags:
  - verify
  - sandbox
  - docker
---

# Verify a backup with the Docker sandbox

> Run `pg_verifybackup` against a freshly-restored copy of a
> backup inside a disposable `postgres:<major>` container.
> Default sandbox backend; one command, no host-side
> PostgreSQL install required. Wallclock: a few minutes plus
> restore time.

## What you need

- A reachable Docker daemon — `testcontainers-go`'s standard
  discovery applies (`DOCKER_HOST` env var or
  `/var/run/docker.sock`).
- Free space under `$TMPDIR` for the restored data dir
  (size of the backup, uncompressed).
- The repo URL the backup lives under, plus the deployment +
  backup ID (or `latest`).
- For encrypted backups: the keystore the agent uses (the
  v0.1 ad-hoc CLI verify path supports plaintext backups
  only — see [Limitations](#limitations)).

## Steps

### 1. Pick a backup

```bash
pg_hardstorage list db1 --repo s3://acme-pg-backups
```

### 2. Run the full verify

```bash
pg_hardstorage verify db1 latest \
    --repo s3://acme-pg-backups \
    --full
```

Equivalent for an explicit backup ID:

```bash
pg_hardstorage verify db1 db1.full.20260427T093017Z \
    --repo s3://acme-pg-backups \
    --full
```

### 3. Inspect the result

```console
✓ verify --full passed (pg_verifybackup on postgres:17)
  Deployment:  db1
  Backup:      db1.full.20260427T093017Z
  Duration:    74321 ms
```

Pipe through `--output json` for machine-parseable output —
the body is the schema-stable `pg_hardstorage.verify.sandbox.v1`
result emitted by the runner.

### 4. (Optional) Override the PG major

The sandbox image defaults to `postgres:<major>` derived from
the backup's `pg_version`. For an image that differs from the
source PG (custom Debian-based mirror, Bitnami repackage):

```bash
pg_hardstorage verify db1 latest \
    --repo s3://acme-pg-backups \
    --full \
    --pg-major 17
```

## What just happened

The CLI restored the backup into a temp directory under
`$TMPDIR`, started a `postgres:<major>` container with the
temp dir bind-mounted at `/var/lib/postgresql/data:ro`, and
exec'd `pg_verifybackup` inside. Container entrypoint is
overridden to `sleep infinity` — the official PG image's own
entrypoint refuses to initialise on top of an existing
`PG_VERSION` file anyway, but the sleep keeps the lifecycle
predictable.

The restore step uses the same `internal/restore` package
the production restore path uses; this is verify-by-replay,
not a parallel implementation. The container is torn down on
completion and the temp dir is removed.

A passing `pg_verifybackup` confirms every page checksum
matches `backup_manifest`, every relation file exists at the
expected length, and the structure is what PG expects to
recover from. A fast verify (`pg_hardstorage verify` without
`--full`) only checks chunk hashes and manifest signatures —
useful, but doesn't tell you the backup is restorable.

## Troubleshooting

### `verify --full: sandbox: ...`

Docker isn't reachable. The error suggestion mirrors what
testcontainers needs: `DOCKER_HOST` set, or a Docker socket
at `/var/run/docker.sock`. Check with:

```bash
docker info
```

Rootless Docker works — point `DOCKER_HOST` at the rootless
socket (`unix://$XDG_RUNTIME_DIR/docker.sock`).

### `verify --full skipped — backup_manifest absent`

The backup was captured without the pg_basebackup manifest
(snapshot-style ingest from a SOURCE plugin that doesn't
emit one). Fast verify still runs over chunk hashes;
sandbox verify is a no-op for these backups. Capture future
backups with the streaming-basebackup pipeline if you need
sandbox verify coverage.

### Out of disk on `$TMPDIR`

Restore stages every byte before the sandbox runs. A 100 GB
backup needs 100 GB free under `$TMPDIR`. Override with
`TMPDIR=/mnt/scratch pg_hardstorage verify ...`.

### Encrypted-backup KEK error

The v0.1 ad-hoc CLI `verify --full` runs with a placeholder
KEK resolver and refuses encrypted backups with a clear
sentinel. The supported path for encrypted backups is the
agent's verify scheduler, which has the keystore wired:

```bash
pg_hardstorage agent  # runs the schedule, including verify
```

See [Verify a backup — fast vs full](../../operations/operator-guide.md)
in the Operator Guide.

## Limitations

- **No `pg_amcheck`.** The sandbox runs `pg_verifybackup` only.
  `pg_amcheck` requires a started cluster, which would need a
  writable copy of PGDATA; that hop ships in v0.5.
- **No smoke SQL.** Same reason: needs a running cluster.
- **No K8s-Job sandbox.** The Backend interface is open; the
  in-tree backends are `docker` and (build-tagged)
  `firecracker`. K8s-Job is a plugin extension point, not on
  this binary's roadmap.
- **Plaintext only via the CLI helper.** Encrypted backups
  flow through the agent's verify scheduler, which holds the
  keystore.

## Next steps

- [Firecracker microVM sandbox](firecracker-sandbox.md) —
  stronger isolation, no Docker daemon dependency.
- [Build the Firecracker variant](../packaging/firecracker-variant.md) —
  the build-tagged binary that ships the microVM backend.
- [Verifier subsystem (SPEC)](../../explanation/architecture-tour.md) —
  the design context behind the two tiers.
- [`verify` CLI reference](../../reference/cli/pg_hardstorage_verify.md)
  — every flag and exit code.
