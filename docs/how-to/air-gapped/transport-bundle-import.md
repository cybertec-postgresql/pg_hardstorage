---
title: Import a repo bundle
description: Receive a bundle on the destination side and write
              it into a repo. Idempotent.
tags:
  - airgap
  - bundle
  - import
---

# Import a repo bundle

> Take a tar produced by `pg_hardstorage repo bundle export`
> on the source side, hand it to a destination repo, and end
> up with the same backups available on the new side.
> Idempotent: re-running on the same bundle is a no-op
> (chunks land via PutIfNotExists; manifests via the same
> conditional-write pattern).

## What you need

- The destination repository URL (must be a valid storage
  scheme: `s3://`, `file://`, `azblob://`, `gcs://`, `sftp://`,
  `scp://`).
- The path to the bundle tar received over the air gap.
- Read access to the bundle, write access to the
  destination repo.

## Steps

### 1. Run the import

```bash
pg_hardstorage repo bundle import \
    --to s3://airgap-mirror/pg-backups \
    --in /mnt/incoming/db1-bundle.tar
```

### 2. Inspect the result

```console
✓ repo bundle import
  Input:        /mnt/incoming/db1-bundle.tar
  Source repo:  s3://acme-pg-backups
  Backups:      1
  Chunks:       1832 (12643 MiB)
  WAL segments: 0
  Note:         idempotent — re-running this command on the same bundle is a no-op (PutIfNotExists)
```

### 3. Verify integrity on the destination

```bash
pg_hardstorage verify db1 latest \
    --repo s3://airgap-mirror/pg-backups
```

### 4. (Optional) Verify with strict signing

The default `Import` is signing-tolerant — useful for
forensic bundles whose source signing key isn't available.
For an environment where the destination must reject any
manifest that doesn't pass signature checks, run a
follow-up:

```bash
pg_hardstorage repo check \
    --repo s3://airgap-mirror/pg-backups
```

(Programmatic callers pass an `ImportOptions.Verifier` to
`bundle.Import`; the CLI tolerant default reflects the
forensic use case.)

## What just happened

`Import` reads the tar in a single pass, dispatching each
entry by path:

| Tar entry prefix | Action |
| --- | --- |
| `bundle.json` | Decoded into the returned `Manifest`. Schema check refuses anything other than `pg_hardstorage.repobundle.v1`. |
| `chunks/…` | `PutIfNotExists` — re-importing skips chunks already present. |
| `manifests/…` | `PutIfNotExists` — manifest, replica, attestation, all conditional. |
| `wal/…` | `PutIfNotExists`. |
| Other | Skipped (forward-compatibility hook). |

The conditional-write posture means a re-run is a free
no-op. A partially-applied import resumes cleanly from
where it stopped.

Two safety gates apply to every entry:

1. **Path-traversal defence.** Entry names that don't
   `path.Clean` to themselves, or that escape via `..`,
   are rejected up-front. Forensic transports cross trust
   boundaries; a malicious bundle cannot write outside the
   destination's prefix.
2. **Size cap.** Any single tar entry larger than 256 MiB
   is rejected (`MaxEntryBytes`). FastCDC chunks are
   ≤ 256 KiB; a 256 MiB cap leaves an enormous safety
   margin while preventing OOM via a maliciously-declared
   10 TB chunk.

## Troubleshooting

### `bundle: rejected entry name "X" (path traversal)`

The bundle's tar contains an entry whose path escapes the
destination prefix. Don't import this bundle; treat it as
hostile and report it to whoever produced it.

### `bundle: entry "X" size N exceeds MaxEntryBytes`

Same security context. A real chunk is ≤ 256 KiB; an entry
larger than 256 MiB is either a misconfigured exporter
(unlikely with our tooling) or hostile. Don't import.

### `bundle: unsupported bundle schema`

The schema check refuses anything other than
`pg_hardstorage.repobundle.v1`. Either the tar isn't a
pg_hardstorage bundle, or it was produced by a future
version with an incompatible layout. Match the binary
versions on source and destination.

### `bundle: archive did not contain bundle.json`

The tar is incomplete (truncated transfer) or not a
pg_hardstorage bundle at all. Re-receive; check the
transfer's integrity (hash + verify on both sides).

### Manifest signature errors after import

`Import`'s default doesn't verify signatures (forensic-
posture). To re-verify on the destination side, run
`pg_hardstorage repo check` over the
deployment. Failures here mean the source was either signed
with a key the destination doesn't trust, or the bundle was
produced from an unsigned source.

## Restoring a backup from an imported bundle

Once `repo bundle import` completes, backups behave like
any other backup in the destination repo:

```bash
pg_hardstorage list db1 --repo s3://airgap-mirror/pg-backups

pg_hardstorage restore db1 latest \
    --repo s3://airgap-mirror/pg-backups \
    --target /var/lib/postgresql/restored
```

PITR works only if the bundle was exported with
`--include-wal`; without WAL the latest snapshot is the
recovery floor. Confirm the WAL count in the import body
matches what you expect.

## Next steps

- [Export a repo bundle](repo-bundle-export.md) — the
  source side.
- [Verify a backup with the Docker sandbox](../verify/docker-sandbox.md)
  — confirm the imported backup actually restores.
- [`repo bundle import` CLI reference](../../reference/cli/pg_hardstorage_repo_bundle_import.md).
- [Operator Guide — restore section](../../operations/operator-guide.md)
  — the full restore + PITR command surface.
