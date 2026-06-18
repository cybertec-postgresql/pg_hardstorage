# plugin/storage/

The storage tier: the object-store backends that hold chunks, manifests, and WAL
segments.

## What lives here

Implementations of the `StoragePlugin` interface — the durable substrate every
backup, WAL segment, and manifest lands on. Each plugin owns its transport (S3
SDK, Azure SDK, SSH, etc.) but conforms to one Go interface, one capability
matrix, and one set of semantics for object lock / WORM / retention.

## StoragePlugin interface

`Put`, `Get`, `Stat`, `List`, `Delete`, `RenameIfNotExists` (atomic write-once),
`SetRetention` (object-lock binding), `Capabilities` (object-lock, server-side
encryption, multipart, etc.), `Close`.

## Plugins

| Name | Scope | Status |
| --- | --- | --- |
| `s3` | AWS S3 + S3-compat (MinIO, Ceph, R2, B2); object-lock + SSE | real |
| `fs` | Local POSIX filesystem (incl. NFS / GlusterFS / Lustre) | real |
| `azblob` | Azure Blob Storage with immutable-blob policies | real |
| `gcs` | Google Cloud Storage with bucket-lock retention | real |
| `sftp` | SFTP server (RFC 4253 / 5656 / 8332 keys) | real |
| `scp` | SCP over SSH; legacy fallback for sites without SFTP | real |
| `faultinject` | Wrapper that injects errors, latency, partial writes | test |
| `throttle` | Wrapper rate-limiting / shaping egress bytes-per-second | real |
| `tls_minio` | TLS-fronted MinIO bootstrap for integration tests | test-only |

## Key files / subdirs

- `storage.go` — the `StoragePlugin` interface + `Capabilities` bitset
- `registry.go` — factory registry
- `contract/` — interface-conformance test harness every plugin's test suite
  runs against

## Read next

- `../../repo/README.md` — the CAS layer that calls this tier
- `../../backup/runner/` — drives `Put` on backup
- `docs/reference/storage-backends.md` — user-facing tuning per backend

## Don't put X here

- Encryption — that's the encryption tier; storage plugins handle ciphertext
  bytes only.
- Compression — same; the chunker handles it.
- Manifest schema or signing — that's `internal/backup/`.
