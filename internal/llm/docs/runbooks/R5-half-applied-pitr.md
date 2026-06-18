# R5 — Half-applied PITR

A PITR restore was running and got interrupted. The target dir is
populated but PG hasn't fully recovered yet, or recovery stopped
short of the target. Either way: don't promote, don't connect
clients, don't panic.

## Symptoms

- The `restore` command died (operator killed it, host rebooted,
  process OOM'd).
- `recovery.signal` exists in the target dir but PG hasn't been
  started, or PG is running and stuck in recovery.
- The PG log shows `archive recovery in progress` followed by
  `LOG: recovery still waiting after ... seconds for WAL` or
  `restore_command failed with exit code 6`.
- A subsequent `pg_hardstorage restore` against the same target
  exits 4 with `preflight.target_not_empty`.

## Pre-flight

- Confirm PG is not promoted yet:

  ```sql
  SELECT pg_is_in_recovery();
  ```

  Must return `true`. If it returns `false`, PG already exited
  recovery and is accepting writes — that is no longer a half-
  applied PITR; either commit to that state or restore again
  cold.

- Confirm what state you're in:

  ```sh
  cat <target>/recovery.signal      # exists if recovery is still pending
  ls <target>/.pg_hardstorage_restore_state.json 2>/dev/null
  ```

  The `.pg_hardstorage_restore_state.json` file (when present)
  records what's been written; a crash mid-restore can resume from
  the last checkpoint rather than starting over.

## Procedure

Pick one of three branches based on what failed.

### Branch A — `restore` died before the verify gate

The restore wrote chunks but didn't get to the verify step. The
data dir is in an indeterminate state.

1. Stop PG if it's running:

   ```sh
   pg_ctl stop -D <target>
   ```

2. Wipe the target:

   ```sh
   rm -rf <target>
   ```

3. Re-run the restore from scratch:

   ```sh
   pg_hardstorage restore <deployment> <backup-id> \
       --target <target> \
       --to "<original-pitr-target>" \
       --repo <url>
   ```

`--force` is unnecessary because the target dir is empty.
Restores are idempotent — chunks already in CAS are not
re-uploaded; the restore re-fetches them locally.

### Branch B — PG started recovery but stopped early

PG is running but `pg_is_in_recovery()` is `true` and isn't
advancing. Most common cause: `wal fetch` returned exit 6 (not
found) before reaching the recovery target — i.e. the WAL the PITR
target depends on isn't in the repo.

1. Check the PG log for the failing segment name. Then verify it
   really isn't in the repo:

   ```sh
   pg_hardstorage wal list <deployment> | grep <segment-name>
   ```

2. If the segment is missing because of a known WAL gap,
   you cannot reach the requested PITR target. Either:

   - Pick a target outside the gap: stop PG, edit
     `postgresql.auto.conf`'s `recovery_target_*` to a value before
     the gap, restart PG.
   - Restore again with a target outside the gap.

3. If the segment SHOULD be in the repo (no known gap), this is a
   chunk-store problem. Treat as
   [R4-repo-corruption-at-rest](R4-repo-corruption-at-rest.md).

### Branch C — verify gate failed (exit 9)

`pg_verifybackup` rejected the data dir. The backup's chunks decode
correctly individually but the assembled directory is wrong —
typically a bug in the restore path, a known WAL gap, or storage-
backend silent corruption.

1. Stop, wipe, restore again. If it fails the same way, the backup
   itself is unrestorable.
2. Tombstone the bad backup:

   ```sh
   pg_hardstorage hold add <deployment> <backup-id> \
       --holder <oncall> --reason "Verify gate failed, audit ref <ticket>"
   ```

3. Pick the previous backup ID (`pg_hardstorage list <deployment>`
   sorted by completion time) and restart from R3-style cold start.

## Verification

- After successful restore: `pg_is_in_recovery()` returns `false`
  after `pg_promote()`.
- `pg_amcheck --all --heapallindexed --rootdescend -d <database>`
  passes.
- The new timeline ID is greater than the manifest's `timeline`.

## Rollback

There is no in-place rollback of a partial PITR. The recovery
mechanism is `rm -rf <target>` and start over. That's why the
restore writes mode-0600 files into a target that pre-flight
required to be empty (or `--force`-cleaned).

## Post-incident

- Append an audit event capturing what failed and which branch
  recovered it.
- If Branch C fired, file a `verify.scrub_mismatch` against the
  affected backup ID and run a full `repair scrub` on the repo.
- If Branch B fired, document the WAL gap window and update SLO
  records — RPO is bounded by the gap.
- Always run `pg_hardstorage doctor` after any half-applied PITR
  to confirm the streaming pipeline is healthy on the new
  endpoint.
