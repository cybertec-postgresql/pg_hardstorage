---
title: Integrity testing
description: The three-layer program that proves backups restore —
              scheduled recovery drills with doctor freshness alarms,
              zero-tolerance storage-contract enforcement, and the
              nightly chaos soak with a restore-proof gate.
tags:
  - integrity
  - drill
  - doctor
  - chaos
  - testing
---

# Integrity testing

A backup that has never been restored is unproven. This page
documents the three layers `pg_hardstorage` uses to keep
"data corruption" and "backup that won't restore" in the class
of bugs that get caught by machines before they reach an
operator:

| Layer | Where it runs | What it proves |
| --- | --- | --- |
| [Scheduled drills](#scheduled-recovery-drills) | Your production agent | The *latest* backup restores, continuously |
| [Contract enforcement](#storage-contract-enforcement) | CI, every commit | Storage backends honour concurrency semantics |
| [Chaos soak](#the-chaos-soak) | Nightly CI | Backups survive failovers, stalls, and races |

The first layer is the one **you** deploy; the other two run in
the project's CI and are documented here so you can reproduce a
failure or run the same proofs against your own environment.

---

## Scheduled recovery drills

The agent can run a full [recovery drill](incident-response.md)
— restore the latest backup into a scratch directory and verify
it — on a schedule, exactly like `backup` and `rotate` tasks:

```bash
pg_hardstorage schedule db1 'daily_at 03:00' --task drill
```

```yaml
deployments:
  db1:
    schedule:
      backup: { every: "6h" }
      rotate: { daily_at: "04:00" }
      drill:  { daily_at: "03:00" }
```

Every drill — pass or fail — appends an entry to the repo's
drill history (`recovery/drills/`), so the evidence lives next
to the backups it proves. A non-pass verdict also fails the
agent task, which surfaces through the agent's normal
task-failure event stream and your
[alerting](alerting-recipes.md).

Convention: drill **after** the typical backup window (backup
at 02:00 → drill at 03:00) so the drill proves the backup the
schedule just took, not yesterday's.

### Doctor: drill freshness

`doctor` reads the drill history and alarms when restorability
is unproven:

| Issue code | Severity | Meaning |
| --- | --- | --- |
| `recovery.drill_never_run` | notice | No drill has ever run for this deployment. |
| `recovery.drill_failing` | critical | The most recent drill did **not** pass. |
| `recovery.drill_stale` | critical | The last *passing* drill is older than `--drill-max-age` (default **7d**). |

```bash
pg_hardstorage doctor db1 --drill-max-age 72h
```

The report body carries a `drills` section per deployment with
`last_verdict`, `last_at`, `last_pass_at`, and `fresh`, so
fleet dashboards can chart drill freshness directly from
`doctor -o json`.

Tune `--drill-max-age` to your drill cadence plus slack: a
daily drill with the default `7d` window tolerates most of a
week of drill failures before escalating — for a strict posture
pair `daily_at` drills with `--drill-max-age 48h`.

---

## Storage contract enforcement

Every storage backend must pass the shared contract suite
(`internal/plugin/storage/contract`), and since this program
landed the **concurrency cases are mandatory**, not opt-in:

- **`ParallelPuts_SingleWinner`** — N goroutines race
  `IfNotExists` puts on one key; exactly one may win and the
  stored bytes must be the winner's. This is the invariant the
  shared-DEK mint, the backup lease, and the audit chain all
  stand on.
- **`ParallelOverwrites_NoTornContent`** — concurrent
  overwrites of one key must never leave torn/interleaved
  content.

The suite is **capability-aware and honesty-enforcing**: a
backend that reports `ConditionalPut: false` skips the
single-winner case (its callers degrade loudly instead — see
below), but a backend that *claims* `ConditionalPut: true` and
fails the race is red. Lying about capabilities is the failure
mode; being honest about a limitation is supported.

### Degrading loudly without conditional put

On backends without atomic `IfNotExists`:

- The backup runner emits a warning event
  (`backup` / `lease_unenforceable`) before taking the lease,
  so overlapping-writer risk is visible in the event stream
  rather than silent.
- The audit chain re-reads every slot it "won" and treats a
  mismatch as a lost race, preserving hash-chain integrity even
  when the backend can't referee the race itself.

### CI demand mode

The contract jobs run with the `DEMAND` variables set, which
turn "environment missing → skip" into "environment missing →
fail" — a contract test can never silently stop running in CI:

```bash
PG_HARDSTORAGE_DEMAND_DOCKER=1   # sftp / MinIO fixtures must start
PG_HARDSTORAGE_DEMAND_SSHD=1     # the real-sshd scp fixture must start
```

The scp backend is exercised against a **real `sshd`** (not a
mock), which is how its production session-open retry under
`MaxSessions` backpressure was found and fixed.

---

## The chaos soak

The nightly `chaos-soak` workflow runs the production model —
an **encrypted** repo, a **continuous** `wal stream`, scheduled
backups, constant write churn — over a real 3-node Patroni
cluster while injecting a seeded random fault schedule:

| Fault | Models |
| --- | --- |
| `switchover` | Planned Patroni failover (the #34 shape) |
| `pause_leader` | GC stall / VM freeze on the primary (3–8 s `docker pause`) |
| `backup_burst` | Two concurrent backups racing the streamer (the #31 shape) |
| `none` | Quiet round — steady-state must also stay clean |

Two rules are encoded from post-mortems:

1. **Processes are never restarted** unless restart *is* the
   injected fault. (A restart-tolerant harness masked the #34
   switchover hang for months.)
2. **The pass criterion is never "exit 0".** After the fault
   budget is spent, the soak enforces a **restore-proof gate**:

   - every committed backup must `verify --full` **and**
     `restore` cleanly,
   - `wal audit` must prove the WAL lineage gap-free across all
     failovers,
   - exactly **one** shared-DEK object may exist in the repo
     (two = the #31 divergent-encryption class),
   - the streamer must still be alive, and stop gracefully on a
     single `SIGINT`.

!!! note "It works"
    The very first soak run caught a real bug: `verify --full` ran
    every backup through a PG18 sandbox regardless of the backup's
    actual major (a `pg_version` parsing slip), so PG17 backups
    failed with `pg_control: CRC is incorrect` despite being fully
    restorable. The restore-proof gate flagged all 32 backups;
    `recovery drill` — which resolves the major correctly — passed
    them, and the diff between the two paths pinpointed the bug.

### Running it yourself

```bash
go build -o /tmp/pghs ./cmd/pg_hardstorage
PGHS_CHAOS_BIN=/tmp/pghs PGHS_CHAOS_MINUTES=6 \
    go test -run TestChaosSoak_RestoreProof -timeout 30m -v \
    ./internal/testkit/topology/
```

Needs Docker and pulls the Spilo + etcd images on first run.
The nightly job uses a 45-minute budget;
`workflow_dispatch` accepts a custom `minutes` input.

### Reproducing a failure

Every run logs its fault-schedule seed:

```console
chaos soak: budget=45m seed=1784835364… (re-run with PGHS_CHAOS_SEED=…)
```

Re-running with `PGHS_CHAOS_SEED=<seed>` (or the workflow's
`seed` input) replays the identical fault sequence, so a red
nightly is a deterministic repro, not a shrug.

---

## Next steps

- [Incident response](incident-response.md) — out-of-cycle
  drills during an incident
- [Schedule backups](../how-to/operating/schedule-backups.md) —
  the `schedule` subcommand, including `--task drill`
- [Alerting recipes](alerting-recipes.md) — wire drill
  failures and doctor criticals into your pager
