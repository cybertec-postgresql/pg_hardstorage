---
title: Point-in-time recovery
description: Replay WAL up to a natural-language timestamp, with a
              preview gate before any byte is written.
tags:
  - pitr
  - restore
  - wal
---

# Point-in-time recovery

> Walks through "I dropped a table five minutes ago, get it back".
> You arm WAL streaming, run the workload, drop the table on
> purpose, and recover state with `--to "5 minutes ago"`.  About
> 15 minutes against a sandbox PG.

This is the tutorial that exercises **the headline feature** of
`pg_hardstorage`: continuous WAL streaming over the replication
protocol.  The base backup you took in
[first backup and restore](first-backup-restore.md) is just the
anchor; what makes recovery byte-precise is the WAL stream that
runs alongside it 24/7.  In production, `pg_hardstorage wal stream`
is the long-running process you supervise with systemd.  Here we
run it in a foreground terminal so you can watch it work.

PITR uses the same `restore` command you used before, with a
`--to`, `--to-lsn`, or `--to-name` target.  WAL is delivered
through a persistent replication slot, so recovery is byte-precise.
The `--preview` flag explains the plan without touching disk —
that is the gate the 3am operator uses to decide whether to
commit.

---

## What you need

- The full setup from [first backup and restore](first-backup-restore.md):
  a sandbox PG, a repo at `file:///tmp/hs-tutorial-repo`, and one
  committed full backup.
- A second terminal — one runs the WAL stream, the other runs psql
  and the restore.

---

## Steps

### 1. Start WAL streaming

In terminal A:

```bash
# RUNNABLE skip-in-ci="indefinite stream / requires multi-terminal sequencing"
pg_hardstorage wal stream db1 \
    --pg-connection "${PG_CONNECTION:-postgres://postgres:postgres@127.0.0.1/postgres}" \
    --repo file:///tmp/hs-tutorial-repo
```

The agent first runs a configuration preflight on the source PG
(checks `wal_level`, `max_replication_slots`, `max_wal_senders`,
the connecting role's `REPLICATION` attribute, and warns on
`max_slot_wal_keep_size` / `idle_replication_slot_timeout`).
Pass `--skip-preflight` to override or run
`pg_hardstorage wal preflight db1 ...` standalone.

Then it issues `CREATE_REPLICATION_SLOT pg_hardstorage_db1
PHYSICAL RESERVE_WAL` (idempotent on an existing slot) — the
`RESERVE_WAL` flag pins the slot's `restart_lsn` to the current
position immediately, so PG retains WAL from this point onwards
even before the first byte of stream traffic.  Finally it issues
`START_REPLICATION SLOT pg_hardstorage_db1 PHYSICAL` and runs an
indefinite receive loop. Each completed 16 MiB segment is
content-addressed and committed atomically; the slot's
`confirmed_flush_lsn` only advances after a segment commits, so a
crash between commits is replayed safely on restart.

Leave it running. `Ctrl-C` shuts it down cleanly.

### 2. Make a change you will want to undo

In terminal B:

```bash
PGPASSWORD=postgres psql -h 127.0.0.1 -U postgres <<'SQL'
CREATE TABLE keep_me  (id int PRIMARY KEY);
CREATE TABLE drop_me  (id int PRIMARY KEY);
INSERT INTO keep_me  SELECT g FROM generate_series(1, 1000) g;
INSERT INTO drop_me  SELECT g FROM generate_series(1, 1000) g;
SELECT now() AS before_drop;
SQL
```

Wait long enough for the WAL to flush — the streamer commits at
segment boundaries, and an idle `pg_switch_wal()` forces one
immediately:

```bash
PGPASSWORD=postgres psql -h 127.0.0.1 -U postgres -c \
    "SELECT pg_switch_wal();"
```

### 3. The mistake

```bash
PGPASSWORD=postgres psql -h 127.0.0.1 -U postgres -c \
    "DROP TABLE drop_me;"
```

Force one more segment so the DROP is committed in the repo:

```bash
PGPASSWORD=postgres psql -h 127.0.0.1 -U postgres -c \
    "SELECT pg_switch_wal();"
```

### 4. Preview the recovery

```bash
# RUNNABLE skip-in-ci="indefinite stream / requires multi-terminal sequencing"
pg_hardstorage restore db1 latest \
    --repo file:///tmp/hs-tutorial-repo \
    --target /tmp/hs-tutorial-pitr \
    --to "5 minutes ago" \
    --preview
```

`--preview` resolves the natural-language time, picks the source
backup, computes the WAL replay range, estimates RTO, and prints the
checklist *without writing anything*:

```console
Restore plan (preview only — no files written)
  Backup:            db1.full.20260504T120000Z.a1b2
  Deployment:        db1
  Target:            /tmp/hs-tutorial-pitr
  PostgreSQL:        17
  Cluster ID:        7659399478052106285
  Backup stop LSN:   0/2000100 (TLI 1)
  Recovery target:   time 2026-05-04T11:55:00Z (inclusive=true)
  On target reached: pause
  Recovery TLI:      latest
  Files:             967
  Total bytes:       22.2 MiB
  Chunk refs:        919 (363 unique, 8.7 MiB after dedup)
  backup_label:      230 bytes
  Estimated RTO:     222 ms (assuming 100.0 MiB/s)
  Pre-flight:        ✓ ready
```

Natural-language parsing supports `<n> minutes/hours/days ago`,
`yesterday`, `today HH:MM`, plain RFC3339, and
`YYYY-MM-DD HH:MM[:SS][±HH:MM]` with a numeric timezone offset.
Numeric offsets with minutes (`+05:30` IST, `+05:45` Nepal,
`-03:30` Newfoundland) are accepted; bare-hour offsets (`+05`)
and the UTC aliases `UTC` / `GMT` / `Z` work too.

Three-letter timezone abbreviations like `IST`, `EST`, `CST` are
deliberately **rejected**: they are ambiguous (IST = India /
Irish / Israel; CST = Central / China) and Go's parser cannot
resolve them safely — accepting them risked a 3am operator
restoring 5–12 hours away from the intended instant.  Always
spell the offset numerically.  Bad input returns
`usage.bad_time` (exit 2) with a suggestion pointing at the
numeric form.

### 5. Apply the recovery

Drop the `--preview` flag:

```bash
# RUNNABLE skip-in-ci="indefinite stream / requires multi-terminal sequencing"
pg_hardstorage restore db1 latest \
    --repo file:///tmp/hs-tutorial-repo \
    --target /tmp/hs-tutorial-pitr \
    --to "5 minutes ago"
```

The command writes the data dir, drops a `recovery.signal`, and
appends a managed `recovery_target_*` block to
`postgresql.auto.conf`. The block's `restore_command` shells back to
`pg_hardstorage wal fetch` so PG can pull WAL from the same repo as
recovery proceeds.

```console
✓ Restore complete
  Backup:        db1.full.20260504T120000Z.a1b2
  Deployment:    db1
  Target:        /tmp/hs-tutorial-pitr
  Files:         967
  Bytes written: 22.2 MiB
  Chunks:        919
  backup_label:  230 bytes
  Duration:      790 ms
  Verification:  passed
  Recovery armed:
    Stop at time: 2026-05-04T11:55:00Z
    Action:       pause
    Timeline:     latest
    Inclusive:    true
```

### 6. Boot the restored cluster and confirm

```bash
docker run --rm -d --name hs-pitr \
    -v /tmp/hs-tutorial-pitr:/var/lib/postgresql/data \
    -p 5434:5432 \
    -e POSTGRES_PASSWORD=postgres \
    postgres:17
```

PG starts, replays WAL up to your `recovery_target_time`, and pauses
(default `--to-action pause`). Confirm both tables are present:

```bash
PGPASSWORD=postgres psql -h 127.0.0.1 -p 5434 -U postgres -c \
    "\dt"
```

```console
 Schema |  Name   | Type  |  Owner
--------+---------+-------+----------
 public | drop_me | table | postgres
 public | keep_me | table | postgres
```

`drop_me` is back. To resume normal operation, run
`SELECT pg_wal_replay_resume();`. To promote out of recovery without
finishing replay, restart the restore with `--to-action promote`.

### 7. Targeting an exact LSN or named restore point

The same command supports two more `--to-*` forms:

```bash
pg_hardstorage restore db1 latest \
    --repo file:///tmp/hs-tutorial-repo \
    --target /tmp/hs-tutorial-pitr \
    --to-lsn 0/1F000028
```

```bash
pg_hardstorage restore db1 latest \
    --repo file:///tmp/hs-tutorial-repo \
    --target /tmp/hs-tutorial-pitr \
    --to-name pre_release
```

Create restore points with `SELECT pg_create_restore_point('pre_release');`
*before* the operation you might want to roll back to.

### 8. Tear down

```bash
docker rm -f hs-pitr
rm -rf /tmp/hs-tutorial-pitr
# Ctrl-C terminal A to stop the WAL streamer.
```

---

## What just happened

You drove a real PITR end-to-end: the streamer committed every
segment to the repo through the persistent slot; the recovery
resolved a natural-language time to a target, planned the operation
under `--preview`, and then committed it under operator control.
Recovery used the in-tree `wal fetch` shim — no `archive_command`
extension required — and the post-restore verifier gated the exit
code.

The two non-obvious wins:

- **`--preview` is the 3am safety net.** Always run it once. It costs
  nothing and surfaces every pre-flight refusal before you commit.
- **Slot-based WAL is gap-free across agent crashes.** PG retains
  segments until the slot ACKs, and the agent only ACKs after a
  segment is committed in the repo. A `kill -9` on the streamer is
  just a restart with no data loss.

---

## Next steps

- [Encryption walkthrough](encryption-walkthrough.md) — recovery still
  works the same when chunks and WAL are AES-GCM-encrypted.
- [R5 — Half-applied PITR](../reference/runbooks/R5-half-applied-pitr.md) —
  what to do when recovery promotes too early or stalls in pause.
- [R6 — Slot dropped, gap detected](../reference/runbooks/R6-slot-dropped-gap.md) —
  diagnosing and repairing a WAL gap.
- [Operator guide — Restore](../operations/operator-guide.md#2-restore) —
  the full restore CLI surface.
