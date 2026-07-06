---
title: Schedule backups
description: Set, show, and clear the backup and rotate schedules
              with the schedule subcommand.
tags:
  - schedule
  - backup
  - rotate
---

# Schedule backups

> Each deployment declares a `backup` and a `rotate` schedule.
> The agent runs both at the configured cadence; the
> `pg_hardstorage schedule` subcommand reads and writes them.

## What you need

- A configured deployment.
- A working `pg_hardstorage agent` instance — the schedules
  fire from there. Without an agent, the schedule sits in YAML
  as configuration only.

## Schedule expressions

Three forms; pick whichever reads cleanest for the cadence
you want:

| Expression | Meaning |
| --- | --- |
| `every 6h` | Every six hours, starting from agent boot. |
| `daily_at 02:00` | Every day at 02:00 UTC. |
| `at 2026-04-28T02:00:00Z` | One-shot at the exact RFC3339 moment. |
| `off` | Clear the schedule (the task no longer fires). |

Durations accept `s`, `m`, `h`, `d`, `w`. `daily_at` always
parses in UTC. We deliberately avoid cron syntax — it's a
bug-magnet and one of the highest-friction surfaces in
backup tooling.

## Steps

### 1. Show all schedules (fleet view)

```bash
# RUNNABLE
pg_hardstorage schedule
```

```console
Schedules for 3 deployment(s):
  DEPLOYMENT  TASK    WHEN
  db1         backup  every 6h0m0s
  db1         rotate  daily at 04:00 (Local)
  db2         backup  daily at 02:00 (Local)
  db2         rotate  daily at 03:30 (Local)
  db3         backup  off
  db3         rotate  off
```

The fleet listing surfaces both tasks — useful for spotting
I/O collisions ("did I schedule two databases to back up at
the same minute?") and missing schedules.

### 2. Show one deployment's schedule

```bash
pg_hardstorage schedule db1
```

```console
Schedule for db1.backup:
  every 6h0m0s
  every:    6h
```

`--task=rotate` shows the rotate schedule instead:

```bash
pg_hardstorage schedule db1 --task=rotate
```

### 3. Set the backup schedule

```bash
pg_hardstorage schedule db1 'every 6h'
pg_hardstorage schedule db1 'daily_at 02:00'
```

The CLI re-parses the expression with the same parser the
agent uses, so a misformatted string is rejected before
landing in YAML.

### 4. Set the rotate schedule

```bash
pg_hardstorage schedule db1 'daily_at 04:00' --task=rotate
```

Convention: rotate **after** the typical backup window so a
just-completed backup doesn't get classified as a candidate by
a sweep that started before it committed.

### 5. Clear a schedule

```bash
pg_hardstorage schedule db1 off
pg_hardstorage schedule db1 off --task=rotate
```

Useful for ad-hoc / test deployments and during maintenance
windows.

## Configuration shape

The YAML representation:

```yaml
deployments:
  db1:
    schedule:
      backup: { every: "6h" }
      rotate: { daily_at: "04:00" }
```

Three keys per task: `every:`, `daily_at:`, or `at:`. Setting
more than one is a config error.

## Coordination across deployments

The agent runs one goroutine per scheduled task; concurrent
backups across deployments run in parallel up to a configurable
concurrency cap (`agent.max_concurrent_backups`, default 4).

To stagger I/O on a single host with multiple deployments,
spread the `daily_at` minutes:

```yaml
deployments:
  db1:
    schedule:
      backup: { daily_at: "02:00" }
  db2:
    schedule:
      backup: { daily_at: "02:15" }
  db3:
    schedule:
      backup: { daily_at: "02:30" }
```

For deeply staggered bursts (every 6h with offset), use
`every: "6h"` — the agent's tick is wall-clock-aligned so
restarts don't reset the cadence.

## Schedules and the LLM helper

`pg_hardstorage llm ask "..."` is the LLM-assisted variant:
given a constraint set ("RPO ≤ 1h, off-peak window 02-05"), it
can suggest a schedule and explain the trade-offs. The schedule
writes themselves still go through `pg_hardstorage schedule`. See
the [`llm` CLI reference](../../reference/cli/pg_hardstorage_llm.md).

## Troubleshooting

**`usage.bad_schedule`** — the expression didn't parse. The
error message echoes the failing token. Common causes: cron
syntax (use `daily_at`), missing duration unit (`6` instead of
`6h`), local-time `daily_at` (it's UTC).

**Schedule is set but backups aren't firing** — the agent
isn't running for that deployment. Check:

```bash
pg_hardstorage doctor db1
systemctl status pg_hardstorage-agent
```

**Two deployments thrash each other's I/O** — stagger the
`daily_at` times, or raise `agent.max_concurrent_backups`
when the box can handle the parallelism.

## Next steps

- [Set retention](set-retention.md) — pair with the rotate
  schedule
- [Verify backups](verify-fast-vs-full.md) — schedule periodic
  verification
- [`schedule` CLI reference](../../reference/cli/pg_hardstorage_schedule.md)
