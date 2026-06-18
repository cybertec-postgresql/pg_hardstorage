# sink

Test-side runtimes for the storage backends — the moral equivalent of
`topology/` but for repos instead of PG clusters.

## What lives here

The agent's storage plugins under `internal/plugin/storage/{azblob,s3,gcs,sftp}`
all work, but for a long time the only repo URL exercised in scenarios was
`file://`. This package fills that gap: each `Runtime` brings up an emulator
container, exposes a URL the agent's storage layer can dial, and tears down on
exit.

Properties:

- Each `Runtime` brings up its own container; no shared state.
- Image tags are pinned in `SinkImages` for reproducibility across time and CI
  environments.
- `pg_hardstorage_testkit images pull-sinks` pre-fetches every image for offline
  runs.
- `--airgap` refuses to fetch missing images at run time, surfacing the gap as a
  pre-flight error rather than a silent network call.

Recent hardening: SSH-banner-wait on the SFTP runtime and Azurite
`CreateContainer` retry both address docker-daemon contention seen in CI
parallelism.

## Key files / subdirs

- `sink.go` — `Runtime` interface, `SinkImages` pinned-tag map, registry
- `azurite.go` — Azure Blob emulator (with retry around `CreateContainer`)
- `minio.go` — S3-compatible emulator
- `tls_minio.go` — MinIO with TLS for HTTPS-URL scenarios
- `gcs_fake.go` — `fake-gcs-server`
- `sftp.go` — `atmoz/sftp` (with SSH-banner-wait hardening)
- `sink_test.go` — Up/Down lifecycle tests against each runtime

## Read next

- `../topology/README.md` — sibling package for PG infrastructure
- `internal/plugin/storage/` — the production-side storage plugins these
  runtimes exercise

## Don't put X here

No production storage code — that's `internal/plugin/storage/`. No repo-layout
knowledge (CAS, manifests, chunks) — sinks only expose a generic URL; the
agent's storage plugin owns the layout.
