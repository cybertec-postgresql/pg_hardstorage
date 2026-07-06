---
title: Verify a backup — fast vs full vs sampled
description: Pick the right verify mode for the question you're
              asking — signature roundtrip, restore-and-pg_verifybackup,
              or a sampled sanity check.
tags:
  - verify
  - integrity
  - pg_verifybackup
---

# Verify a backup — fast vs full vs sampled

> Three verification modes, each suited to a different
> question. Run **fast** routinely; run **full** before you
> trust a backup with a production restore; run **sampled** /
> **existence-only** as a 100x-cheaper sanity check on cold
> archives.

## Modes at a glance

| Mode | Reads | Decrypts | Restores | pg_verifybackup | Typical use |
| --- | --- | --- | --- | --- | --- |
| Fast (default) | every chunk | yes | no | no | Hourly / daily cron |
| Sampled (`--sample N`) | first N chunks in hash-sorted order | yes | no | no | Spot-check a cold archive |
| Existence-only (`--existence-only`) | metadata only (Stat) | no | no | no | Pre-flight before `backup undelete` |
| Full (`--full`) | every chunk | yes | yes (sandbox) | yes | Quarterly / pre-DR drill |

## Fast verify

The default. Validates the manifest's Ed25519 signature, then
for each referenced chunk: `Get` from the CAS, decrypt with the
resolved KEK, recompute SHA-256 over the plaintext, compare to
the manifest entry. No PG, no restore, no WAL replay.

```bash
# RUNNABLE
pg_hardstorage verify db1 latest
```

```console
verify db1/db1.full.20260427T020000Z (full)
  manifest signature: valid
  chunks: 3245 referenced, 3245 unique, 3245 sampled
  ✓ 3245 chunk(s) verified — 12.3 GiB in 9412ms
```

`--repo <url>` overrides the deployment's configured repo for
ad-hoc verification of off-site replicas.

Mismatch → `verify.chunk_mismatch` (exit 9). The manifest's
signature must verify too — a tampered manifest fails before
the chunk loop runs.

## Sampled verify

```bash
pg_hardstorage verify db1 <backup-id> --sample 1000
```

Caps the chunk count to the first N chunks in hash-sorted
order — deterministic, so repeated runs check the same subset
and results are reproducible. Useful for cheap periodic sanity
checks on cold archives where running the full verify weekly is
overkill.

`--sample 0` and omitting the flag both mean "full chunk
list."

## Existence-only

```bash
pg_hardstorage verify db1 <backup-id> --existence-only
```

`Stat`s every unique chunk instead of fetching it. Useful as a
pre-flight before [`backup undelete`](../../reference/cli/pg_hardstorage_backup_undelete.md)
to confirm chunk-GC hasn't reclaimed the body, or as a 100x-
cheaper sanity check on cold archives. **Mismatch on bytes is
NOT detected** in this mode — only absence.

## Full verify

The end-to-end verifier: restore the backup into a sandbox PG,
run `pg_verifybackup` against it, and confirm the cluster
opens. The sandbox is a Docker container by default; for the
isolated microVM variant, see the Firecracker sandbox how-to
(coming soon).

```bash
pg_hardstorage verify db1 <backup-id> --full
```

`--full` requires Docker on the host. For a control-plane
deployment (the agent runs the sandbox, not your laptop):

```bash
pg_hardstorage verify db1 <backup-id> \
    --control-plane https://control.acme.example.com \
    --control-plane-ca /etc/pg_hardstorage/control-ca.pem
```

`--control-plane` always implies `--full` semantics — the
network round-trip would be wasted on a fast verify that runs
locally.

## Restore-time auto-verify

Every `pg_hardstorage restore` also runs `pg_verifybackup`
post-restore by default — that integration is the canonical
"the bytes are correct" check.

```text
--verify=auto      # default; runs pg_verifybackup if on $PATH
--verify=require   # error if pg_verifybackup is missing
--verify=skip      # skip; only after acknowledging exit 9 contract
```

`pg_verifybackup` ships in PostgreSQL 13+; on older clusters
the auto-mode silently skips the verify and the agent's
journal logs a notice.

## Repository scrub

Verify-the-whole-repo, not one backup:

```bash
pg_hardstorage repo scrub <repo-url>
pg_hardstorage repo scrub <repo-url> --sample-percent 10
pg_hardstorage repo scrub <repo-url> --full          # 100% pass
```

Wire `repo scrub --sample-percent 1` into an hourly cron;
quarterly run `--full` for the exhaustive pass. See
[Scrub and heal](scrub-and-heal.md).

## Picking a cadence

Recommended baseline:

| Cadence | Mode | What it catches |
| --- | --- | --- |
| Per backup (auto) | post-restore `pg_verifybackup` | clean restore can't open |
| Hourly | `repo scrub --sample-percent 1` | bit-rot on hot chunks |
| Daily | `verify <deployment> latest` (fast) | manifest tamper, chunk SHA mismatch |
| Weekly | `verify <deployment> latest --full` | end-to-end DR proof |
| Quarterly | `repo scrub --full` | bit-rot in cold tail |

## Troubleshooting

**`verify.chunk_mismatch`** — chunk bytes don't hash to the
expected SHA-256. Either the storage backend corrupted them
(scrub the rest of the repo with [scrub-and-heal](scrub-and-heal.md))
or the chunk was tampered with.

**`verify.chunks_missing`** — a referenced chunk is absent.
Could be: a partial replicate, an over-aggressive `repo gc`,
or storage-backend data loss. Pair with
`pg_hardstorage repair manifest` and a replica.

**`verify.manifest_signature`** — the manifest's Ed25519
signature is invalid. Treat as untrusted. Either the signing
key changed and the manifest was never re-signed, or the
manifest was modified after commit. Cross-check with the
[audit chain](../../operations/operator-guide.md#8-audit-log).

**`usage.no_pg_verifybackup`** — `--verify=require` set but
the binary's not on `$PATH`. Install
`postgresql-client-<ver>` or upgrade to PG 13+.

## Next steps

- [Scrub and heal](scrub-and-heal.md) — bit-rot detection +
  auto-heal from replica
- [Rotate the KEK](rotate-kek.md) — `kms verify` is the
  envelope-only counterpart to fast verify
- [`verify` CLI reference](../../reference/cli/pg_hardstorage_verify.md)
- [Runbook R4: repo corruption at rest](../../reference/runbooks/R4-repo-corruption-at-rest.md)
