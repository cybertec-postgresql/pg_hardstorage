---
title: Export a repo bundle
description: Bundle a deployment's manifests, chunks, optional WAL,
              and timeline files into one tar for air-gap transport.
tags:
  - airgap
  - bundle
  - export
---

# Export a repo bundle

> Stream every manifest a deployment owns plus the chunks
> they reference into a single deterministic tar file you
> can carry across the air gap on a USB drive, an
> approved-content gateway, or a one-way diode.  The output
> is plain tar — every air-gapped network already passes
> through it, every operator can `tar tvf` to inspect.

## What you need

- The source repository URL (where the backups live today).
- A path on local disk with enough free space for the
  bundle (≈ chunk-bytes plus a small overhead). Compression
  is **not** baked in; pipe through `gzip` / `zstd`
  yourself if you want it.
- The deployment name. Optional: a single backup ID to
  scope the export.

## Steps

### 1. Bundle every live backup for one deployment

```bash
pg_hardstorage repo bundle export \
    --repo s3://acme-pg-backups \
    --deployment db1 \
    --out /mnt/transport/db1-bundle.tar
```

### 2. Bundle one specific backup

```bash
pg_hardstorage repo bundle export \
    --repo s3://acme-pg-backups \
    --deployment db1 \
    --backup-id db1.full.20260427T093017Z \
    --out /mnt/transport/db1-2026-04-27.tar
```

### 3. Include WAL for PITR on the destination

```bash
pg_hardstorage repo bundle export \
    --repo s3://acme-pg-backups \
    --deployment db1 \
    --include-wal \
    --out /mnt/transport/db1-bundle-with-wal.tar
```

`--include-wal` pulls every WAL segment listed in each
manifest's `wal_required` field, plus the corresponding
timeline-history files. Without it the destination can
restore the backup but cannot replay WAL.

### 4. Inspect the bundle without unpacking

```bash
tar -xOf /mnt/transport/db1-bundle.tar bundle.json | jq .
```

`bundle.json` is the bundle's table of contents:

```json
{
  "schema": "pg_hardstorage.repobundle.v1",
  "generated_at": "2026-05-04T08:13:42Z",
  "source_repo": "s3://acme-pg-backups",
  "backups": [
    { "deployment": "db1", "backup_id": "db1.full.20260427T093017Z", "type": "full" }
  ],
  "wal": [],
  "chunk_count": 1832,
  "chunk_bytes": 13245612544
}
```

## What just happened

`Export` walked every live (non-tombstoned) manifest in the
deployment, copied the manifest, its replica copy,
attestation (when present), and every chunk it references
into the tar, then appended `bundle.json` at the end. The
on-disk repo layout is preserved exactly inside the tar so
import is "untar onto destination repo" plus integrity
checks.

The bundle is the right surface for forensics on a repo
whose signing key is unavailable — `Export` reads manifests
without forcing a signature check (the destination's
`Import` is where verification lives, not the source's
read).

The output **is not compressed**. Chunks already carry the
storage-layer compression posture (zstd / lz4 / none), so
double-compressing hurts more than it helps. Pipe through
`gzip` or `zstd` yourself if your transport medium needs
it.

## Cross-account replication versus repo bundles

Both move backups across boundaries. Different shapes:

| Need | Pick | Why |
| --- | --- | --- |
| Live, near-real-time mirror with consent | `repo replicate` | Bilateral ACL, push from source. |
| Batched offline transport, one-way | `repo bundle export` + `import` | Tar; survives diodes, USB drops, approval workflows. |
| Forensic copy where source signing key is gone | `repo bundle export` | Read path doesn't require signed manifests. |

## Troubleshooting

### `--out` already exists

`Export` opens the output with `O_EXCL` — won't clobber an
existing file. Pick a new path or remove the old bundle
deliberately.

### `repo bundle export: no manifests for deployment ...`

Either the deployment name is wrong (`pg_hardstorage status`
to list them) or every backup is tombstoned. Tombstones
hide manifests from `List` until `repo gc` sweeps the
chunks. To export a tombstoned manifest you need to
un-tombstone it first; ad-hoc forensic tooling for that
ships in v0.5.

### Bundle is huge

The bundle carries every chunk by hash, deduplicated within
the bundle itself (each chunk hash appears at most once).
You're seeing post-dedup, post-compression bytes — there
isn't more compression to extract.

If transport size is the constraint, scope to one backup
with `--backup-id`, or omit `--include-wal` (WAL is the
biggest single contributor for write-heavy deployments).

### Half-tarred file at the output path

If `Export` errors mid-stream the output file is removed
on best-effort cleanup. A truly hard-killed `pg_hardstorage`
(SIGKILL) may leave a partial tar; remove it and re-run.

## Reproducibility

`Export` sorts entries deterministically: backups by
`(deployment, backup_id)`, WAL by filename, chunks by
hash. Two exports over the same repo state produce
byte-identical bundles. Useful for CI evidence and for
operators who hash the bundle into an audit log.

## Next steps

- [Import a repo bundle](transport-bundle-import.md) — the
  destination side.
- [Air-gap policy](enable-policy.md) — what's allowed
  outbound under strict mode.
- [`repo bundle export` CLI reference](../../reference/cli/pg_hardstorage_repo_bundle_export.md)
  — every flag.
- [Cross-account replication](../../operations/operator-guide.md)
  — the live-mirror alternative.
