---
title: Apply a legal hold
description: Pin a backup against retention sweeps for an
              indefinite or time-bounded litigation window.
tags:
  - hold
  - retention
  - litigation
---

# Apply a legal hold

> A legal hold pins one backup against retention regardless of
> policy outcome. Held manifests refuse `backup delete` and are
> skipped by `rotate`; the only way to remove a hold is the
> explicit `hold remove` operation.

## What you need

- A reachable repository.
- A backup ID (use `pg_hardstorage list <deployment>` to find
  it).
- The free-form *holder* identifier (operator email, ticket
  number, lawyer's name) and *reason* for the hold — both land
  in the marker file and in a tamper-evident audit-chain event
  (`hold.add` on placement, `hold.remove` on release), so a
  compliance reviewer can reconstruct every hold action from
  `pg_hardstorage audit search`.

## Steps

### 1. List existing backups

```bash
pg_hardstorage list db1
```

```console
Backups for db1 (3):
  BACKUP ID                  TYPE  WHEN              FILES  SIZE      DEDUP  DURATION
  db1.full.20260427T020000Z  full  2026-04-27 02:00  1284   12.3 GiB  1.42x  38210ms
  db1.full.20260420T020000Z  full  2026-04-20 02:00  1281   12.1 GiB  1.41x  37544ms
  db1.full.20260413T020000Z  full  2026-04-13 02:00  1279   11.9 GiB  1.40x  36988ms
```

### 2. Place an indefinite hold

```bash
pg_hardstorage hold add db1 db1.full.20260420T020000Z \
    --repo file:///srv/pg_hardstorage/repo \
    --holder ops@acme.example.com \
    --reason "GDPR Art 17 #4421 — pending legal review"
```

```console
hold placed
  deployment:   db1
  backup_id:    db1.full.20260420T020000Z
  holder:       ops@acme.example.com
  reason:       GDPR Art 17 #4421 — pending legal review
  expires_at:   (none — indefinite)
  marker_path:  holds/db1/db1.full.20260420T020000Z.json
```

The marker is a sibling file in the repo, so it survives agent
restarts and is visible across operators.

### 3. Place a time-bounded hold

`--until` accepts duration shorthand or absolute time:

```bash
pg_hardstorage hold add db1 db1.full.20260420T020000Z \
    --repo file:///srv/pg_hardstorage/repo \
    --holder ops@acme.example.com \
    --reason "Debug window for investigation #1234" \
    --until 14d
```

```bash
pg_hardstorage hold add db1 db1.full.20260420T020000Z \
    --repo file:///srv/pg_hardstorage/repo \
    --holder counsel@acme.example.com \
    --reason "Litigation period — Smith v. Acme" \
    --until 2027-01-01
```

After the expiry the marker stays on disk for audit but no
longer protects the manifest. Run [`hold purge-expired`](#5-purge-expired-holds)
periodically to keep the marker directory tidy.

### 4. List active holds

```bash
pg_hardstorage hold list --repo file:///srv/pg_hardstorage/repo
```

```console
DEPLOYMENT  BACKUP_ID                       HOLDER                   REASON                                  EXPIRES
db1         db1.full.20260420T020000Z       ops@acme.example.com     GDPR Art 17 #4421 — pending legal …    (indefinite)
db1         db1.full.20260413T020000Z       counsel@acme.example.com Litigation period — Smith v. Acme       2027-01-01
db2         db2.full.20260415T020000Z       counsel@acme.example.com Tax audit 2026                          2026-12-31
```

`--repo` is required. Pass a deployment name positionally to
filter:

```bash
pg_hardstorage hold list db1 --repo file:///srv/pg_hardstorage/repo
```

### 5. Purge expired holds

```bash
# Preview first (no mutations, no audit emits):
pg_hardstorage hold purge-expired --repo file:///srv/pg_hardstorage/repo --dry-run

# Then actually remove:
pg_hardstorage hold purge-expired --repo file:///srv/pg_hardstorage/repo --yes
```

`--yes` is required for the live removal (`--dry-run` is the only
way to run it without `--yes`). Removes every marker whose `ExpiresAt` is in the past. The
markers themselves are gone, but the underlying manifest is now
eligible for rotation (it was never deleted; just protected).

### 6. Remove a hold

The only way to clear an active hold:

```bash
pg_hardstorage hold remove db1 db1.full.20260420T020000Z \
    --repo file:///srv/pg_hardstorage/repo \
    --yes
```

The `--yes` is mandatory — clearing a hold is the irreversible
side of the marker, and it should be deliberate.

## How holds interact with retention

`pg_hardstorage rotate --apply` filters held backups before
the soft-delete sweep:

```console
✓ Rotation applied
  Policy: gfs

  db1
    keep:    3
    delete:  0
    held:    2 (excluded from delete: db1.full.20260420T020000Z, db1.full.20260413T020000Z)
    applied: 0
```

The result body's `held` / `held_ids` fields capture what
got skipped — useful for a compliance auditor reconstructing
"why is this backup still here despite the policy?"

## How holds interact with crypto-shred

A held backup is **not** protected against [crypto-shred](crypto-shred.md).
The shred destroys the KEK; every backup wrapped under it
becomes unrecoverable, including held ones. Holds protect
against retention sweeps; they don't override key destruction
— that's the point of crypto-shred under GDPR Art. 17.

If a held backup must remain readable, it must be wrapped
under a different KEK than the one slated for destruction.
Per-tenant KEKs (configured at the deployment level) are the
clean answer.

## Marker shape

```json
{
  "schema": "pg_hardstorage.hold.v1",
  "deployment": "db1",
  "backup_id": "db1.full.20260420T020000Z",
  "holder": "ops@acme.example.com",
  "reason": "GDPR Art 17 #4421 — pending legal review",
  "placed_at": "2026-04-28T14:21:08Z",
  "placed_by": "ops@host01",
  "expires_at": null
}
```

Stored at `holds/<deployment>/<backup-id>.json` under the repo
root. Schema-versioned (`pg_hardstorage.hold.v1`); future
fields are additive.

## Troubleshooting

**`hold.already_held`** — the backup already has an active
marker. Use `hold list` to see the current holder/reason.

**`hold.not_held`** on remove — no marker present (already
removed, or never placed). Idempotent — no harm done.

**`rotate` removed a backup that should have been held** —
confirm the marker existed at rotate time. The marker is a
write-then-fsync to the repo, so a marker placed *after* a
running rotate may have raced. Re-place the hold and add a
fresh backup if the targeted manifest is lost.

## Next steps

- [Set retention](set-retention.md) — pair with holds for the
  full lifecycle picture
- [Crypto-shred](crypto-shred.md) — what holds **don't**
  defend against
- [`hold` CLI reference](../../reference/cli/pg_hardstorage_hold.md)
