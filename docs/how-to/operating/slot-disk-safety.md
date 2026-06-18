---
title: Replication-slot disk safety
description: How to balance WAL durability against pg_wal/ disk-fill risk
              when a streamer disconnects, using max_slot_wal_keep_size
              and slot-lag alerting together.
tags:
  - wal
  - operations
  - slot
  - disk
---

# Replication-slot disk safety

> A physical replication slot tells PostgreSQL to *retain* WAL
> until the streamer consumer ACKs it. That is exactly what
> `pg_hardstorage` wants — it is also the reason an abandoned
> streamer can fill `pg_wal/` until the server runs out of disk.
> This page documents the trade-off and the recommended
> configuration for typical and tier-0 deployments.

## What you need

- A configured deployment.
- The connecting role can read PG settings (`SELECT current_setting(...)`).
- A monitoring stack capable of scraping the `pg_hardstorage_*`
  Prometheus metrics, or a way to act on `doctor` output.

## Why this matters

PostgreSQL retains every WAL segment whose LSN is at or after the
slot's `restart_lsn` for as long as the slot exists, even when
the slot is *inactive*. If the streamer stays disconnected for an
extended period, those segments accumulate in `pg_wal/`. When
the partition holding `pg_wal/` fills up, PostgreSQL stops
accepting writes. The slot has done its job exactly as designed,
but the production database is unavailable.

There are two policies a deployment can choose between:

- **Slot retains all WAL** (`max_slot_wal_keep_size = -1`, the
  PG default): zero WAL loss as long as the slot exists; an
  abandoned slot can fill the partition.
- **Capped slot retention** (`max_slot_wal_keep_size = N MB`):
  disk usage is bounded; an outage longer than `N MB` of WAL
  silently loses everything past the cap.

Neither default is universally safe. The right answer is to
**pair one with the matching alert** so the safety net catches
the failure before it becomes an incident.

## Recommended configurations

### Most deployments — bound the slot, alert on lag

For the typical case — a managed PG or a single primary where
losing a few MB of WAL after a sustained outage is acceptable —
configure `max_slot_wal_keep_size` to a value larger than the
worst-case streamer downtime you can tolerate, and alert before
the cap is reached.

```ini
# postgresql.conf
max_slot_wal_keep_size = 32GB
```

Pair with a Prometheus alert on the streamer's lag:

```
ALERT pg_hardstorage_wal_lag_high
  IF pg_hardstorage_wal_archive_lag_bytes > 24 * 1024 * 1024 * 1024
  FOR 5m
  LABELS { severity = "warning" }
  ANNOTATIONS { summary = "WAL lag is 24 GB — within 8 GB of the slot cap" }
```

Sizing rules of thumb:

- Worst-case downtime × write rate = WAL volume. Round up by a
  factor of two and add a safety margin.
- The alert threshold should fire well before the cap so an
  on-call has time to act (default: 75 % of the cap).
- The cap should be at most 50 % of the partition holding
  `pg_wal/`, so an unrelated checkpoint or backup-induced WAL
  spike does not fill the disk on its own.

### Tier-0 — leave the slot unbounded, alert on disk and lag

If WAL durability matters more than disk safety — regulated
deployments, the kind where any loss after a clean primary is
unacceptable — leave `max_slot_wal_keep_size` at the default
(`-1`, unlimited) and accept the disk-fill risk. Operate the
streamer as a hard requirement: an abandoned slot is an incident,
not a steady state.

```ini
# postgresql.conf
max_slot_wal_keep_size = -1
```

Pair with two alerts:

- **Streamer-lag alert** — fire when `pg_hardstorage_wal_archive_lag_seconds`
  crosses a threshold (e.g. 60 s for hot deployments).
- **Disk-free alert** on the partition holding `pg_wal/` — fire
  before disk usage crosses 80 %, with a page at 90 %.

### Patroni clusters — `permanent_slots` plus the cap

In Patroni clusters with `permanent_slots`, the slot exists on
every node; an unexpected leader change does not orphan it.
Treat slot disk safety as a property of every member:

- Set the same `max_slot_wal_keep_size` cap on every node so a
  replica that becomes leader inherits the same policy.
- Use the dual-slot mode (`physical_wal.mode: dual`) to avoid
  losing the slot on a single failover.

## How pg_hardstorage helps you choose

`pg_hardstorage wal preflight` reports the current setting on
every preflight pass:

```sh
pg_hardstorage wal preflight <deployment> \
    --pg-connection 'postgres://pgbackup@db.example.com/postgres'
```

- `max_slot_wal_keep_size.set` — a warning when the value is
  positive. The risk being flagged is *WAL loss*: PG will recycle
  WAL even when the slot is behind. Pair the cap with a
  streamer-lag alert and confirm the cap exceeds any backup
  window.
- `max_slot_wal_keep_size.unbounded` — an informational note when
  the value is `-1` (the PG default). The risk being flagged is
  *disk fill*. Pair with a disk-free alert on the partition
  holding `pg_wal/`.

Both findings are present so the policy is visible at install
time and at every doctor pass; neither prevents the streamer from
running.

## Day-2 workflow when a slot has been abandoned

If `pg_wal/` is growing and the slot is the cause:

1. **Confirm the slot is the cause.** `SELECT slot_name,
   pg_size_pretty(pg_wal_lsn_diff(pg_current_wal_lsn(),
   restart_lsn)) AS slot_keeps FROM pg_replication_slots;`.
2. **Try to drain the slot.** Start (or restart) the
   `pg_hardstorage` agent so the streamer reconnects and ACKs.
   The retained WAL drops as the streamer catches up.
3. **If the agent cannot recover the slot** — run
   `pg_hardstorage wal repair <deployment>`. The slot is dropped
   and recreated from the latest backup's stop LSN, accepting
   whatever gap that creates. The gap auditor records it; PITR
   into the window is refused.
4. **Take a fresh backup** under the new slot anchor. New backups
   are independent of the gap and re-establish a restore floor.

`pg_hardstorage doctor <deployment>` confirms the recovery —
the slot should be active, lag dropping.

## What `pg_hardstorage doctor` watches for you

- `wal.lag` — slot lag in seconds and bytes.
- `wal.gap` — the gap auditor's verdict per timeline.
- `wal.slot` — slot present, active, and owned by the expected
  consumer.
- `wal.preflight` — re-runs the preflight on each cycle.

A `doctor` exit with a `wal.*` finding plus the alerts above is
the recipe that catches both failure modes early.

## Read next

- [WAL pipeline](../../explanation/wal-pipeline.md) — the
  streaming and gap-auditor architecture.
- [R6 — Slot dropped during failover, gap detected][r6] —
  recovery when the slot is already gone.

[r6]: ../../reference/runbooks/R6-slot-dropped-gap.md
- [Alerting recipes](../../operations/alerting-recipes.md) —
  prebuilt Prometheus rules for both alert shapes above.
- [Monitoring](../../operations/monitoring.md) — the
  `pg_hardstorage_wal_*` metric reference.
