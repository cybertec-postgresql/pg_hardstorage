# R3 — Cold start from backups

PostgreSQL is gone. Host destroyed, data dir deleted, primary and
replica both down. All you have is the repository and the keyring.
This is the canonical "we lost the database" runbook and the most
important one in this directory.

## Symptoms

- The primary host is unreachable, refuses to boot, or the
  `pg_data` directory is empty / corrupt.
- `psql` to the production endpoint refuses connections or returns
  garbage.
- You're at a fresh host (or the same host post-repaved) and need
  to reconstruct PG from the repo.

## Pre-flight

Before touching anything, verify the inputs you have:

1. **Repository is reachable and intact.**

   ```sh
   pg_hardstorage repo check <repo-url>
   ```

   The output must be clean — signatures verify, chunks present.
   If `repo check` reports findings, see
   [R4-repo-corruption-at-rest](R4-repo-corruption-at-rest.md)
   first; do not restore from a corrupt repo.

2. **Keyring is present and matches the backups.**

   ```sh
   pg_hardstorage kms inspect
   ```

   Note the public-key fingerprint. It must match the manifest's
   `attestation.public_key` for the backup you intend to restore.
   If the fingerprint doesn't match, you have the wrong keyring;
   find the right one before continuing. A wrong keyring fails fast
   at restore-time (`kms.key_missing`, exit 8) — but verify now,
   not after a 4-hour copy.

3. **Pick the restore target.**

   - Backup ID. `pg_hardstorage list <deployment>` for choices.
     Pick the latest one that pre-dates the loss event by enough
     margin to avoid ingesting whatever caused the loss.
   - PITR target if you have one (e.g. just before a known-bad
     deploy at `2026-04-29 10:42 UTC`).
   - Target host with enough free disk for the restored data dir
     plus 20% margin. Pre-flight asserts 110%; aim for more.

4. **Verify pg_verifybackup is on PATH.**

   ```sh
   which pg_verifybackup
   ```

   The mandatory verify gate uses it. If it isn't there, install
   the matching `postgresql-client-<major>` package or pass
   `--verify=skip` at restore-time (only after explicit
   acknowledgement — exit 9 is the contract).

## Procedure

1. **Provision the target host.** Install PG of the same major
   version the backup was taken against (the manifest's
   `pg_version` field). Do NOT initialise the data dir — `pg_hardstorage
   restore` does that:

   ```sh
   pg_hardstorage show <deployment> <backup-id> | jq '.result.body.pg_version'
   ```

2. **Preview before committing.** This prints the WAL replay range,
   RTO estimate, target tablespace mapping, verification gate. No
   bytes move:

   ```sh
   pg_hardstorage restore <deployment> <backup-id> \
       --target /var/lib/postgresql/restored \
       --to "<your-pitr-target>" \
       --repo <repo-url> \
       --preview
   ```

   Read the output. Confirm the LSN range falls outside any known
   WAL gaps (the preview will refuse if not). Confirm the
   tablespace mapping matches what you expect on this host.

3. **Run the restore.** Same flags, drop `--preview`. Pass
   `--force` if the target directory is non-empty (the default
   refuses to overwrite):

   ```sh
   pg_hardstorage restore <deployment> <backup-id> \
       --target /var/lib/postgresql/restored \
       --to "<your-pitr-target>" \
       --repo <repo-url>
   ```

   The restore writes mode-0600 files, fsyncs per file, atomically
   commits recovery files (`recovery.signal` and a managed block
   in `postgresql.auto.conf` whose `restore_command` invokes
   `pg_hardstorage wal fetch`). The mandatory `pg_verifybackup`
   gate runs against the data dir before the command declares
   success. Exit 9 means the verifier said no; do not start PG.

4. **Start PG.** systemd unit, `pg_ctl start`, container, whatever
   your local convention is. PG enters recovery, `wal fetch` is
   invoked per segment until the recovery target is reached or no
   more WAL is available (exit 6 from `wal fetch` is what tells
   PG to stop).

5. **Wait for recovery completion.** Watch the PG log for
   `consistent recovery state reached` and then `recovery has paused
   at WAL location ...` (if you specified a target) or `archive
   recovery complete` (if you restored to latest).

6. **Promote.** When PG is paused at the recovery target:

   ```sql
   SELECT pg_promote();
   ```

   PG exits recovery and accepts writes on a new timeline.

## Verification

- PG is up, accepting connections, on a new timeline ID greater
  than the manifest's `timeline`.
- `SELECT pg_is_in_recovery();` returns `false`.
- `pg_amcheck --all --heapallindexed --rootdescend` passes (run
  this; it is the strongest check we have):

  ```sh
  pg_amcheck --all --heapallindexed --rootdescend -d <database>
  ```

- A spot-check query against your most-recently-known data is
  consistent with the PITR target.
- `pg_hardstorage status <deployment>` shows the recovered host as
  the new endpoint and starts a fresh backup chain.

## Rollback

- If the restore fails verification (exit 9): the data dir is in a
  partially-populated state. `rm -rf` the target dir. Pick a
  different backup ID and restart from step 2.
- If recovery on PG fails after start: stop PG, `rm -rf` the data
  dir, repeat the restore. Restores are idempotent on the target
  path with `--force`.
- If you promoted prematurely: there is no rollback from
  `pg_promote()`. Take a fresh backup of whatever state you ended
  up in and start a new run if needed.

## Post-incident

- Document the timeline: when did the loss happen, when did the
  restore start, when did PG accept writes again, what RPO and RTO
  were achieved.
- Compare to SLO targets:

  ```sh
  pg_hardstorage slo report <deployment>
  ```

- Append an audit event: `restore.cold_start_completed` with
  source backup ID, PITR target, new timeline ID.
- Re-establish the streaming pipeline against the new endpoint:
  `pg_hardstorage init` (idempotent on the deployment) reconfigures
  the slot and resumes WAL streaming under a new persistent slot
  on the new timeline.
- File a postmortem against the originating loss event. The cold
  start succeeded; the question is why it was needed.
