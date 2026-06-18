# Zalando (Spilo) argv fixtures

Captured by running `spilo-argv-recorder:17` (an overlay of
`ghcr.io/zalando/spilo-17:4.0-p1` that swaps in the
`argv-recorder` binary in place of `wal-g`) inside a
Zalando `postgres-operator` 1.13.0 deployment with Spilo's
WAL-G archiving config.

## Files

- `argv-fixtures.ndjson` — 10 unique `wal-g` invocations
  captured from real Spilo archive_command + scheduled
  backup activity.
- `argv-fixtures-raw.ndjson` — full unfiltered capture
  including all environment variables (kept for diff against
  future Spilo versions).
- `manifests/discovery.yaml` — Namespace + MinIO + postgresql
  CRD that produces the captures.

## What Spilo actually invokes

Three distinct verbs across all 10 captures:

```
wal-g backup-push <pgdata-path>
wal-g backup-list
wal-g wal-push   <pg_wal/WAL_FILE>
```

All configuration arrives via env vars (no flags):

| env var | purpose |
|---|---|
| `WALG_S3_PREFIX` / `WALE_S3_PREFIX` | destination bucket + prefix |
| `AWS_ENDPOINT` | S3 endpoint URL (custom; MinIO here) |
| `AWS_REGION` | S3 region |
| `AWS_S3_FORCE_PATH_STYLE` | path-style bucket addressing |
| `AWS_ACCESS_KEY_ID` + `AWS_SECRET_ACCESS_KEY` | credentials |
| `WALG_DOWNLOAD_CONCURRENCY` / `WALG_UPLOAD_CONCURRENCY` | parallelism knobs |

The archive_command Spilo writes into postgresql.conf is:

```
envdir "/run/etc/wal-e.d/env" wal-g wal-push "%p"
```

## End-to-end drop-in verification

Our existing `pg-hardstorage-walg` shim handles **all three**
verbs Spilo invokes.  Once the repo is initialised
(`pg_hardstorage repo init <s3-url>`), Spilo's archive_command
fires our shim, which translates env→native and produces
genuine pg_hardstorage objects in the bucket:

```
HSREPO                              # repo manifest
chunks/sha256/<aa>/<bb>/<...>.chk   # FastCDC-chunked content
wal/<deployment>/<timeline>/<wal>.json  # segment manifest
```

PG's `pg_stat_archiver.archived_count` ticks up; verified by
running `wal-g wal-push` manually inside the pod and inspecting
MinIO via `mc ls -r m/<bucket>/`.

## Known caveat for v1: repo must be pre-initialised

Real WAL-G auto-creates its bucket structure on first push;
our shim returns `notfound.repo` if the repo manifest doesn't
exist.  For the K8s drop-in scenario to work without manual
intervention, one of:

1. The shim's `wal-push` / `backup-push` should auto-init the
   repo when missing (most operator-friendly).
2. The K8s scenario YAMLs include a one-shot Job that runs
   `pg_hardstorage repo init` before the postgresql cluster
   starts archiving.

Tracked in a follow-up task; v1 of the Zalando smoke
scenario uses option 2.
