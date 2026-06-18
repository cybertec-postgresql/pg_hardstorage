---
title: Scrub and heal
description: Detect bit-rot with a sampling scrub, then auto-heal
              mismatched chunks from a replica repo.
tags:
  - scrub
  - bit-rot
  - heal
  - integrity
---

# Scrub and heal

> Storage backends silently corrupt bytes. Scrub re-hashes
> chunks against their content-address; mismatches are the
> "your storage backend has lost coherence" signal. With a
> replica configured, `repair scrub --heal` re-fetches the
> good bytes and rewrites the local copy.

## What you need

- A reachable repository.
- For the `--heal` path: a replica repo populated by
  [`pg_hardstorage repo replicate`](../../reference/cli/pg_hardstorage_repo_replicate.md).
  The replica's chunk envelopes are byte-identical to the
  primary (replicate copies verbatim), so a heal restores the
  local copy to a state where the CAS plaintext-SHA round-trip
  passes again.

## Steps

### 1. Hourly: cheap sampling scrub

```bash
# RUNNABLE
pg_hardstorage repo scrub file:///srv/pg_hardstorage/repo --sample-percent 1
```

```console
scrub  repo=file:///srv/pg_hardstorage/repo  sample=1%
       chunks_checked=412   mismatches=0   ok
```

Wire this into a cron job:

```text
0 * * * * pg_hardstorage repo scrub file:///srv/pg_hardstorage/repo --sample-percent 1
```

Mismatches map to exit 9, so a cron-wired scrub alarms when
integrity slips. Findings land in the
[hash-chained audit log](../../operations/operator-guide.md#8-audit-log).

### 2. Quarterly: full scrub

```bash
pg_hardstorage repo scrub file:///srv/pg_hardstorage/repo --full
```

`--full` is shorthand for `--sample-percent 100`. Every
referenced chunk gets fetched and re-hashed. Plan for the
read-bandwidth this costs; on object stores it's a real bill.

### 3. Diagnose with `repair scrub`

The same scrub semantics, more output:

```bash
pg_hardstorage repair scrub --repo file:///srv/pg_hardstorage/repo --limit 1000
```

`--limit 0` means full scrub. The findings include the
backup IDs that reference each mismatched chunk — useful when
you want to know which backups are at risk before deciding
how to remediate.

### 4. Heal mismatches from a replica

```bash
pg_hardstorage repair scrub \
    --repo file:///srv/pg_hardstorage/repo \
    --heal --replica file:///srv/pg_hardstorage/repo-replica \
    --limit 0
```

```console
chunks_checked=12453  mismatches=2  healed=2  not_at_replica=0
  healed: sha256=abc123... refetched=4 KiB
  healed: sha256=def456... refetched=64 KiB
```

Heal is **best-effort**: a chunk missing at the replica is
reported as `NotAtReplica` and the run continues with the next
mismatch. To preview without writing, run scrub **without**
`--heal` — it reports every mismatch (exactly what a heal would
repair) and touches nothing:

```bash
pg_hardstorage repair scrub \
    --repo file:///srv/pg_hardstorage/repo \
    --limit 0
```

### 5. After healing — verify

```bash
pg_hardstorage repo check file:///srv/pg_hardstorage/repo
pg_hardstorage verify db1 latest --repo file:///srv/pg_hardstorage/repo
```

`repo check` confirms manifest signatures and chunk references;
`verify db1 latest` re-runs the SHA round-trip on the
just-healed bytes.

## Cadence

Recommended baseline:

| Repo character | Sample scrub | Full scrub |
| --- | --- | --- |
| Hot (writes weekly+) | hourly 1% | quarterly 100% |
| Warm (writes monthly) | daily 5% | quarterly 100% |
| Cold (archive) | daily 1% | annually 100% |

Each row is the floor; compliance regimes (PCI, HIPAA, SOC 2)
typically push you up a tier.

## Mismatches without a replica

Without `--heal --replica`, scrub is a diagnostic only — the
mismatch is reported and the audit chain captures it, but the
local copy stays bad. Steps:

1. Pull the original backup from cold storage / off-site
   replica.
2. `pg_hardstorage repo replicate` it back into the primary
   repo.
3. Re-run `repair scrub --heal` to fix the now-recoverable
   mismatches.

If no replica exists at all, the affected backups are lost.
The mismatched chunk's plaintext SHA is in the audit chain;
attach that to your incident postmortem.

## What scrub doesn't catch

- **Manifest tampering with valid signature** — rare, but
  possible if the signing key was compromised. `kms verify`
  is the corresponding envelope check.
- **Wrong KEK** — the chunk decrypts to garbage; SHA mismatch
  surfaces it as `verify.chunk_mismatch` and you'd misdiagnose
  it as bit-rot. `kms verify` distinguishes envelope vs
  payload.
- **Restore-time correctness** — the bytes can be hash-correct
  and the cluster still won't open due to PG-internal
  issues. That's what
  [full verify](verify-fast-vs-full.md#full-verify) catches.

## Troubleshooting

**`scrub.unreachable`** — the repo URL doesn't resolve.
Confirm credentials / network.

**`heal.NotAtReplica` for every mismatch** — the replica
isn't fully populated, or it was cut from a cold archive that
predates the corrupted chunks. Re-run `repo replicate` to
top up.

**Heal runs but `repo check` still fails** — there's a deeper
problem (missing manifest, broken signature, GC collision).
See [Runbook R4: repo corruption at rest](../../reference/runbooks/R4-repo-corruption-at-rest.md).

## Next steps

- [Verify a backup](verify-fast-vs-full.md)
- [`repo scrub` CLI reference](../../reference/cli/pg_hardstorage_repo_scrub.md)
- [`repair scrub` CLI reference](../../reference/cli/pg_hardstorage_repair_scrub.md)
- [Runbook R4: repo corruption at rest](../../reference/runbooks/R4-repo-corruption-at-rest.md)
