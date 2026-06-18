---
title: Set a retention policy
description: Configure GFS, simple, or count retention — with WORM
              for the regulatory case.
tags:
  - retention
  - rotate
  - gfs
---

# Set a retention policy

> Three policies ship today: **GFS**
> (grandfather-father-son), **simple** (everything younger than
> N), and **count** (last N fulls). The newest backup is
> **always** kept regardless of policy output — a misset flag
> never leaves you with zero backups.

## What you need

- A configured deployment.
- A reachable repository (the policy applies per-repo).

## Policy options

### GFS — the default

```yaml
deployments:
  db1:
    retention:
      policy: gfs
      keep_daily: 7      # one backup per UTC day, last 7 days
      keep_weekly: 4     # one per ISO week, last 4 weeks
      keep_monthly: 12   # one per UTC month, last 12 months
      keep_yearly: 5     # one per UTC year, last 5 years
```

Best when your retention story has the shape "recent backups
in fine grain, older backups in coarse grain." A single backup
can satisfy multiple buckets at once (the youngest backup of
the week is also the youngest of the day) — the policy keeps
each bucket's nominee at most once.

### Simple — keep everything younger than X

```yaml
retention:
  policy: simple
  keep_for: 30d        # keep every backup younger than this duration
```

Best when storage is cheap and you'd rather not reason about
calendar buckets. Use durations: `30d`, `2w`, `12h`, `45m`.

### Count — keep the last N fulls

```yaml
retention:
  policy: count
  keep_full_count: 14
```

WAL is kept while needed for PITR onto any retained full.
Best when you size storage by full-backup count and the WAL
volume is relatively predictable.

## Steps

### 1. Edit `pg_hardstorage.yaml`

Add or update the `retention:` block under the deployment.

### 2. Preview the decision (dry-run)

```bash
# RUNNABLE
pg_hardstorage rotate db1 --repo file:///srv/pg_hardstorage/repo
```

```console
deployment: db1     policy: gfs
   kept     id                              reason
    ✓       db1.full.20260427T020000Z       newest (always kept)
    ✓       db1.full.20260426T020000Z       daily slot 1
    ✓       db1.full.20260420T020000Z       weekly slot 1
    ✗       db1.full.20260419T020000Z       superseded by 20260420
   ...
```

`rotate` defaults to dry-run. Read the table; nothing's
happened yet.

### 3. Apply

```bash
pg_hardstorage rotate db1 \
    --repo file:///srv/pg_hardstorage/repo \
    --apply
```

`--apply` writes a `<manifest>.json.tombstone` marker beside
each soft-deleted manifest, and records one `backup.rotate_delete`
audit-chain event per deleted backup (the policy in the body) so a
post-incident review can reconstruct exactly which backups a
retention run removed. List operations filter the tombstoned IDs;
reads return `ErrTombstoned`. Chunks stay
until [`repo gc`](../../reference/cli/pg_hardstorage_repo_gc.md)
sweeps them — making retention reversible until the next GC.

### 4. (Optional) One-off override

CLI flags override the YAML for a single run:

```bash
pg_hardstorage rotate db1 \
    --repo file:///srv/pg_hardstorage/repo \
    --policy simple --keep-for 14d --apply
```

```bash
pg_hardstorage rotate db1 \
    --repo file:///srv/pg_hardstorage/repo \
    --policy count --keep-fulls 5 --apply
```

```bash
pg_hardstorage rotate db1 \
    --repo file:///srv/pg_hardstorage/repo \
    --policy gfs \
    --keep-daily 7 --keep-weekly 4 --keep-monthly 12 --keep-yearly 5 \
    --apply
```

The YAML stays unchanged.

## Regulatory retention (WORM)

Retention policies are *advisory* — the binary skips listed
backups, but a sufficiently privileged operator can still
delete them. For a regulatory-grade posture, set WORM at
`repo init`:

```bash
pg_hardstorage repo init 's3://acme-pg-backups/?region=eu-central-1' \
    --worm-mode compliance \
    --worm-retention 7y
```

`compliance` mode survives even root credentials until the
deadline. WORM is set **at init time only** — flipping it on
later produces a mixed-fleet situation operators can't reason
about. See [Add an S3 repository](../adding/repository-s3.md#3-aws-s3-with-worm-object-lock).

## Safety net

- The newest manifest is **always** kept regardless of policy
  output. Even `--keep-for 1m` (one minute) cannot leave the
  deployment with zero backups.
- `[Legal holds](legal-hold.md)` filter held backups out of
  the rotation set — a retention sweep cannot remove a
  held manifest.
- `--apply` writes tombstones, not actual deletes; a misset
  flag remains reversible until `repo gc`.

## Scheduled rotation

Rotation runs automatically after every backup commit and on
the schedule the deployment declares:

```yaml
deployments:
  db1:
    schedule:
      backup: { every: "6h" }
      rotate: { daily_at: "04:00" }
```

See [Schedule backups](schedule-backups.md) for the schedule
expression grammar.

## Troubleshooting

**`rotate.no_repo`** — `--repo` not set and the deployment
config has no `repo:`. Add it.

**`held` backups in the result body** — those are skipped by
design. See [Legal hold](legal-hold.md).

**Decisions look wrong** — check the deployment's timezone vs
UTC. GFS buckets are cut on UTC midnight; a 23:50 backup
"yesterday" UTC-wise is the next day's bucket if you read in
local time.

## Next steps

- [Schedule backups](schedule-backups.md)
- [Apply a legal hold](legal-hold.md)
- [Crypto-shred](crypto-shred.md) — irreversible destruction
  for GDPR Art. 17
- [`rotate` CLI reference](../../reference/cli/pg_hardstorage_rotate.md)
