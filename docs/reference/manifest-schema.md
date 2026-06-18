<!-- AUTO-GEN candidate: reflect over backup.Manifest struct tags; per docs/DOC_PLAN.md auto-generation map. -->
---
title: Manifest schema
description: On-disk backup manifest — every field, type, optionality, and the canonicalisation-for-signing rule.
tags:
  - reference
  - manifest
  - backup
---

# Manifest schema

Every backup commits a single **manifest** — the
single-source-of-truth document describing the backup's
chunks, files, encryption envelope, and signed
attestation.  The schema string is
`pg_hardstorage.manifest.v1`; 24-month back-compat
applies.

Source: [`internal/backup/manifest.go`](https://github.com/cybertec-postgresql/pg_hardstorage/blob/main/internal/backup/manifest.go).

## Stability rules

- **Marshal order is field-declaration order.**
  `encoding/json` is deterministic for structs; we rely on
  that for canonical-bytes equality across signer and
  reader.
- **No `map[K]V` fields anywhere.**  Map iteration is
  non-deterministic; introducing one would break
  signature round-trips.
- **`Canonicalize()` zeros `Attestation` and re-marshals.**
  The signer signs those bytes; the verifier reconstructs
  them.  `SetEscapeHTML(false)` so `< / > / &` survive
  verbatim.

## Top-level: `Manifest`

| Field (JSON) | Go type | Required | Notes |
| --- | --- | --- | --- |
| `schema` | string | yes | Must equal `pg_hardstorage.manifest.v1` |
| `backup_id` | string | yes | Stable opaque ID; the manifest's primary key |
| `deployment` | string | yes | Logical deployment name |
| `tenant` | string | no | Multi-tenant slot |
| `type` | string | yes | `full`, `incremental_lsn`, or `snapshot` |
| `parent_backup_id` | string | no | Required for `incremental_lsn`; identifies the parent in the chain |
| `pg_version` | int | yes | Server-major (e.g. `16`, `17`) |
| `system_identifier` | string | yes | PG `system_identifier` from `pg_control` |
| `start_lsn` | string | yes | Backup-start LSN |
| `stop_lsn` | string | yes | Backup-stop LSN |
| `timeline` | uint32 | yes | PG timeline ID at backup time |
| `started_at` | RFC3339 timestamp | yes | UTC |
| `stopped_at` | RFC3339 timestamp | yes | UTC |
| `compression` | string | no | Codec name (`zstd`, `gzip`, `noop`); empty = uncompressed |
| `encryption` | object | no | See [`EncryptionInfo`](#encryptioninfo); absent = plaintext |
| `tablespaces` | array | no | Non-default tablespaces; see [`Tablespace`](#tablespace) |
| `files` | array | yes | Ordered file inventory; see [`FileEntry`](#fileentry) |
| `wal_required` | array of strings | no | WAL segment names needed for restore |
| `backup_label` | string | no | Verbatim PG `backup_label` |
| `tablespace_map` | string | no | Verbatim PG `tablespace_map`; empty when the cluster has only the default tablespace |
| `pg_backup_manifest` | bytes (base64) | no | Verbatim PG `backup_manifest`; **required** to anchor an incremental child on PG 17+ |
| `wal_gaps` | array | no | Patroni-failover WAL gaps detected on or before commit; see [`WALGap`](#walgap) |
| `attestation` | object | no | Set by `Sign`; zeroed during canonicalisation. See [`Attestation`](#attestation) |

## `EncryptionInfo`

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `scheme` | string | yes | Envelope scheme name |
| `kek_ref` | string | yes | [KEKRef](kekref-schemes.md) — `local:default`, `aws-kms://…`, etc. |
| `wrapped_dek` | string (base64) | yes | The per-backup DEK wrapped by the KEK |
| `envelope_version` | int | yes | Envelope-format version; bumps on incompatible layout changes |

`encryption: null` (or absent) means the chunks are
plaintext — possible with the `noop` encryption codec or in
testing setups, never the production default.

## `Tablespace`

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `oid` | uint32 | yes | PG OID of the tablespace |
| `location` | string | yes | Filesystem path at backup time. Restore consults this to remap |

## `FileEntry`

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `path` | string | yes | Repo-relative path inside PGDATA |
| `size` | int64 | yes | Bytes |
| `mode` | uint32 | no | POSIX mode bits |
| `mod_time` | RFC3339 timestamp | no | mtime at backup time |
| `chunks` | array | yes | Ordered list of `ChunkRef`; concatenation is the file's bytes |

## `ChunkRef`

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `hash` | repo.Hash | yes | Content-addressed chunk hash (CAS key) |
| `offset` | int64 | yes | Byte position within `FileEntry` |
| `len` | int64 | yes | Chunk length; matches CAS-stored size for unencrypted chunks |

## `WALGap`

A strict subset of `gapstate.Record`.  Embedded in the
manifest so:

- restore can refuse PITR within the gap window even
  when live gapstate has been GC'd / wiped;
- the manifest is signed — the gap record cannot be
  tampered with after commit;
- cross-region replicas of the manifest carry the
  gap metadata for free.

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `slot_name` | string | yes | Replication slot the gap was detected on |
| `slot_role` | string | no | `primary` / `replica` |
| `timeline` | uint32 | yes | Timeline at gap-detection time |
| `gap_start_lsn` | string | yes | LSN where the gap opens |
| `gap_end_lsn` | string | yes | LSN where the gap closes |
| `gap_bytes` | uint64 | yes | Estimated unrecoverable bytes |
| `detected_at` | RFC3339 timestamp | yes | UTC |

Restore consults **both** this field and live
gapstate; either source can refuse a PITR target inside
the window with `restore.target_in_wal_gap`.

## `Attestation`

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `scheme` | string | yes | Today: `ed25519` (`SchemeEd25519`) |
| `public_key` | string (PEM) | yes | Embedded so the manifest is self-verifying |
| `signature` | string (base64) | yes | Raw signature bytes over `Canonicalize()` output |

## Canonical bytes (signing payload)

`Manifest.Canonicalize()` returns the deterministic bytes
the signer signs and the verifier reconstructs:

1. Make a shallow copy.
2. Set `Attestation = nil`.
3. Encode with `json.Encoder`, `SetEscapeHTML(false)`,
   no indentation.
4. Trim the trailing newline `Encode` appends.

`MarshalToBytes()` produces the on-disk form using the
same encoder settings — so the on-disk file's bytes
*minus* the attestation block are bit-identical to the
canonical signing bytes.

## Read paths

| Function | Verifies signature? | Use when |
| --- | --- | --- |
| `ParseAndVerify(raw, verifier)` | yes | Production reads; the only public-key-trusted entry point |
| `ParseAttestationless(raw)` | no | `repair attestation` / `repair manifest` only — paths whose entire purpose is to handle broken signatures |

`ParseAndVerify` errors:

| Error | Meaning |
| --- | --- |
| `ErrUnsigned` | Manifest has no `attestation` block |
| `ErrPublicKeyMismatch` | Signature is genuine but the embedded public key differs from the verifier's; not signed by a trusted party |
| `manifest: unsupported signature scheme "…"` | `attestation.scheme` is not `ed25519` |

## See also

- [KEKRef schemes](kekref-schemes.md) — possible
  values of `encryption.kek_ref`.
- [Storage URL schemes](storage-url-schemes.md) — where
  manifests live on the wire.
- [Output event schema](output-event-schema.md) — the
  surface that surfaces `verify.manifest_signature`
  failures.
- [Plugins → Encryption (KMS) contract](plugins/encryption-contract.md) —
  what `encryption.scheme` selects.
