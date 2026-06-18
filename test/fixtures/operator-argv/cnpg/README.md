# CNPG argv fixtures

Captured by running `cnpg-argv-recorder:17` (an overlay of
`ghcr.io/cloudnative-pg/postgresql:17` that swaps in our
`argv-recorder` binary) inside CNPG 1.29.0 against an
in-cluster MinIO sink, then triggering both ambient WAL
archiving and a one-shot `Backup` CRD.

## Files

- `argv-fixtures.ndjson` — one JSON line per unique tool
  invocation captured.  Volatile fields (timestamps, PIDs,
  Kubernetes service env vars) are filtered to keep diffs
  meaningful.
- `manifests/discovery.yaml` — Namespace + MinIO + Cluster
  CRD that produces the captures.  Reproduce with:

  ```sh
  kubectl apply -f manifests/discovery.yaml
  kubectl apply -f manifests/discovery-backup.yaml
  ```

- `manifests/discovery-backup.yaml` — one-shot Backup CRD
  that triggers `barman-cloud-backup`.

## What the fixtures tell us

CNPG 1.29.0 invokes three of the eight `barman-cloud-*`
binaries at runtime.  The argv shapes are:

### barman-cloud-wal-archive

```
barman-cloud-wal-archive \
    --gzip \
    --endpoint-url <S3-endpoint> \
    --cloud-provider aws-s3 \
    <s3://destination/path> \
    <stanza-name> \
    <pg_wal/WAL_FILE>
```

Fired by the postgres pod's `archive_command` for every WAL
file rotation.  Credentials come from
`AWS_ACCESS_KEY_ID` + `AWS_SECRET_ACCESS_KEY` env vars.

### barman-cloud-wal-restore

```
barman-cloud-wal-restore \
    --endpoint-url <S3-endpoint> \
    --cloud-provider aws-s3 \
    <s3://destination/path> \
    <stanza-name> \
    <WAL_NAME> \
    <output-path>
```

Fired by replicas' `restore_command` to fetch missing WAL
during streaming-replication catchup.  No `--gzip` flag
(decompression is implicit on the read path).  The 6th
positional arg is a *bare* WAL name, NOT prefixed with
`pg_wal/`.

### barman-cloud-backup

```
barman-cloud-backup \
    --user postgres \
    --name <backup-id> \
    --gzip \
    --endpoint-url <S3-endpoint> \
    --cloud-provider aws-s3 \
    <s3://destination/path> \
    <stanza-name>
```

Fired by the `Backup` CRD's reconciler.  `--name` carries the
operator-assigned backup label (e.g. `backup-20260508101757`).
Connects to PG via the local Unix socket (no `--host` /
`--port`); `--user postgres` overrides the default.

## Important: barman-cloud is being deprecated by CNPG

CNPG 1.29.0 emits a deprecation warning at apply time:

> Native support for Barman Cloud backups and recovery is
> deprecated and will be completely removed in CloudNativePG
> 1.30.0. Please migrate existing clusters to the new Barman
> Cloud Plugin to ensure a smooth transition.

CNPG 1.30+ will move backup orchestration out of the postgres
image into a separate **plugin** sidecar.  The plugin protocol
is defined upstream; our drop-in story for CNPG 1.30+ will
need to ship a plugin implementation rather than a binary
overlay.  The fixtures here remain useful for CNPG 1.29.x and
older where the in-image barman-cloud-* path is still active.

For 1.30+, see `test/fixtures/operator-argv/cnpg-plugin/`
(landing once the plugin protocol is sampled).
