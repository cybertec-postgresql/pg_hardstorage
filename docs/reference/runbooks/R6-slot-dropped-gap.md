# R6 — Slot dropped during failover, gap detected

A Patroni failover happened, the new leader doesn't have our
replication slot, and the gap auditor flagged that we lost some WAL.
Streaming has resumed against the new leader but PITR within the
gap window is now refused.

## Symptoms

- `pg_hardstorage doctor` reports `wal_gap_detected` for the
  deployment.
- `wal stream` logs include a `wal.slot_recreated` notice with
  non-zero `gap_bytes` or `gap_seconds`.
- An attempted PITR within the gap window fails with
  `wal.gap_in_target` (exit 6).
- Patroni's REST endpoint shows a recent leader change.

## Pre-flight

- Confirm scope of the gap:

  ```sh
  pg_hardstorage wal list <deployment> --repo <url> -o json | jq '.result.body.gaps'
  ```

  Each gap entry has `start_lsn`, `end_lsn`, and the timeline IDs
  it spans.

- Confirm streaming is back up against the new leader:

  ```sh
  pg_hardstorage doctor <deployment>
  ```

  The slot should be active, lag dropping.

- Confirm the new leader is configured to host the slot
  correctly.  `wal preflight` validates `wal_level`,
  `max_replication_slots`, `max_wal_senders`, the connecting
  role's `REPLICATION` attribute, plus warnings on
  `max_slot_wal_keep_size` and `idle_replication_slot_timeout`.
  A misconfigured replica that just got promoted is the most
  common cause of a *second* slot loss right after the first:

  ```sh
  pg_hardstorage wal preflight <deployment> \
      --pg-connection 'postgres://pgbackup@new-leader/postgres'
  ```

  Address every fatal finding before re-enabling the streamer
  unit, otherwise the slot will be re-recreated on the next
  start with the same vulnerability.

## Procedure

The gap is real WAL loss — async replication didn't ship those
records before the old primary went down. There is no recovery for
data inside the gap. Your job is to:

1. **Document the gap** so future restore attempts understand why
   the bound exists. The gap auditor records it automatically;
   confirm with:

   ```sh
   pg_hardstorage wal list <deployment> --repo <url> | grep -i gap
   ```

2. **Take a fresh backup** under the new timeline. New backups are
   independent of the gap and serve as a safe restore floor:

   ```sh
   pg_hardstorage backup <deployment>
   ```

3. **Update SLO accounting.** Compare the gap span to your RPO
   target:

   ```sh
   pg_hardstorage slo report <deployment>
   ```

   If the gap exceeds RPO, this is an SLO miss event — escalate
   per organisational policy.

4. **If you need a restore that falls inside the gap window:** the
   gap is a hard refusal. The closest restorable points are the
   LSN immediately before the gap (on the old timeline) or the
   first LSN after the gap (on the new timeline). Pick whichever
   is acceptable for the use case.

   ```sh
   pg_hardstorage restore <deployment> <backup-id> \
       --target <path> \
       --to-lsn <last-lsn-before-gap> \
       --repo <url> --preview
   ```

5. **Prevent recurrence.** Configure Patroni `permanent_slots` so
   the next failover doesn't drop the slot:

   ```yaml
   # patroni.yml
   slots:
     pg_hardstorage_<deployment>:
       type: physical

   permanent_slots:
     pg_hardstorage_<deployment>:
       type: physical
   ```

   With `permanent_slots`, Patroni recreates the slot on every
   leader and propagates `restart_lsn` via slot-advance — residual
   gap is at most one Patroni cycle of WAL, typically < 100 MB.

   Until Strategy A lands fully (v0.5+), edit Patroni's config and
   reload. `pg_hardstorage doctor` rechecks every cycle and clears
   the warning when the slot exists on every node.

## Verification

- `pg_hardstorage doctor <deployment>` is clean except for the
  documented gap entry.
- `pg_hardstorage status <deployment>` shows WAL lag near zero
  against the new leader.
- A test restore to a target outside the gap completes and passes
  the verify gate.
- Patroni's `GET /cluster` confirms `permanent_slots` carry our
  slot name.

## Rollback

Not applicable — the gap is data loss, not a state change. The
fresh backup taken in step 2 should not be rolled back; if it had
problems, see [R4-repo-corruption-at-rest](R4-repo-corruption-at-rest.md).

## Post-incident

- Append an audit event with the gap's `start_lsn`, `end_lsn`, and
  the trigger (Patroni failover).
- Decide: was the failover planned? If unplanned, file a Patroni
  diagnostic against whatever caused the leader change.
- If you weren't running `permanent_slots`, this is a recurrence-
  prevention win. If you were, the gap shouldn't have appeared —
  file a `pg_hardstorage` bug with the doctor output and Patroni
  cluster state.
- For tier-0 deployments, evaluate dual-slot mode (`physical_wal:
  mode: dual`, two slots on two nodes streaming concurrently) —
  the duplicate chunks cost nothing because of CAS dedup, and a
  failover doesn't drop both slots at once.
