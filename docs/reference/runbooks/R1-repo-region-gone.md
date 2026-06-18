# R1 — Repo region is gone

The primary repository region is down or unreachable. The agent has
been failing chunk uploads with `storage.unreachable` (exit 8). You
need to keep PG protected and switch to a replica region.

## Symptoms

- `pg_hardstorage doctor` reports `repository unreachable` for one
  or more deployments.
- `wal stream` exits with `storage.unreachable` and won't restart
  cleanly. PG is still serving writes but `pg_wal/` on the primary
  is growing because our slot is holding WAL.
- Cloud provider status page shows the region degraded.

## Pre-flight

- A replica repo exists. Check the `repo.replicate_to` config or
  ask whoever set up cross-region replication.
- The replica region has a copy of every manifest needed for
  current restores. `repo check` against the replica:

  ```sh
  pg_hardstorage repo check <replica-url>
  ```

- You have credentials for the replica region.

## Procedure

1. **Stop the streamer** so PG isn't blocked indefinitely.

   ```sh
   systemctl stop pg_hardstorage
   # or send SIGTERM to the wal stream process
   ```

2. **Repoint the deployment** at the replica region. Edit
   `pg_hardstorage.yaml`:

   ```yaml
   deployments:
     db1:
       repo: <replica-url>            # was the primary url
   ```

3. **Reset the slot** so streaming resumes against the new repo.
   This will create a WAL gap covering the region-outage window;
   the gap auditor records it.

   ```sh
   pg_hardstorage wal repair db1
   ```

4. **Take a fresh backup** under the replica repo. New backups are
   independent of the gap and serve as a safe restore floor.

   ```sh
   pg_hardstorage backup db1
   ```

5. **Restart streaming** against the replica.

   ```sh
   systemctl start pg_hardstorage
   ```

## Verification

- `pg_hardstorage doctor` is clean against the replica.
- `pg_hardstorage status db1` shows WAL lag dropping; PG's
  `pg_wal/` size starts decreasing.
- `pg_hardstorage list db1 --repo <replica-url>` shows the new
  backup at the top.

## Rollback

When the primary region recovers:

1. Repoint config back to the primary (`repo: <primary-url>`).
2. `pg_hardstorage wal repair db1` to reset the slot.
3. Take one backup. It will dedup against any chunks already in the
   primary region.
4. Restart streaming.

The replica region is now lagging until the next replication
sweep — keep `repo.replicate_to` configured both ways if you want
two-way redundancy.

## Post-incident

- File a `wal_gap_detected` record against the deployment for the
  outage window.
- Append an audit event documenting the failover and the affected
  LSN range.
- If the gap span exceeds your RPO target, escalate per
  organisational policy.
- Update SLO records:

  ```sh
  pg_hardstorage slo report db1
  ```
