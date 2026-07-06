# R4 — Repo corruption at rest

A scrub or `verify` flagged chunk(s) whose plaintext SHA-256 doesn't
match the manifest. Bit-rot, backend corruption, or tamper. Your job
is to localise the damage, repair what's repairable, and quarantine
what isn't.

## Symptoms

- `pg_hardstorage repair scrub` or
  `pg_hardstorage verify <d> <id>` exits 9 with code
  `verify.scrub_mismatch` or `verify.chunk_mismatch`.
- `pg_hardstorage repo check <url>` reports
  `verify.missing_chunks` or signature failures on one or more
  manifests.
- A restore against a specific backup fails the verify gate.

## Pre-flight

- Confirm scope: which chunks, which manifests, which backups.
  The error body lists the offending chunk hashes.
- Check whether a replica region is configured and reachable.
  `repo check` against it:

  ```sh
  pg_hardstorage repo check <replica-url>
  ```

- Stop any in-flight backup that might be writing into the same
  affected paths.

## Procedure

1. **Localise.** Get the full list of corrupt chunks and the
   manifests that reference them:

   ```sh
   pg_hardstorage repair scrub --repo <url> -o json | tee /tmp/scrub.ndjson
   pg_hardstorage repair chunks --missing --repo <url> -o json | tee /tmp/missing.json
   ```

2. **If a replica exists, auto-heal from it.** The fastest path is
   `pg_hardstorage repair scrub --heal --replica <replica-url>`,
   which re-fetches every mismatched chunk from the replica and
   rewrites it locally (use `--dry-run-heal` first to preview).

   To heal by hand instead, for each corrupt chunk hash copy
   the corresponding object from the replica region's
   `chunks/sha256/aa/bb/aabb<rest>.chk` to the same path under
   the primary region. Fail-safe because writes are CAS — the
   `If-None-Match: *` precondition means a stale local object
   prevents the replacement; remove the bad object first:

   ```sh
   # filesystem repo
   rm <primary>/chunks/sha256/aa/bb/aabbcc...chk
   cp <replica>/chunks/sha256/aa/bb/aabbcc...chk <primary>/chunks/sha256/aa/bb/

   # s3 repo
   aws s3 rm s3://<primary-bucket>/chunks/sha256/aa/bb/aabbcc...chk
   aws s3 cp s3://<replica-bucket>/chunks/sha256/aa/bb/aabbcc...chk \
             s3://<primary-bucket>/chunks/sha256/aa/bb/
   ```

3. **If either copy of a manifest is corrupt** — primary or
   replica — reconcile the two from whichever side is intact:

   ```sh
   pg_hardstorage repair manifest <deployment> <backup-id>
   ```

   The command repairs in whichever direction is needed: a
   corrupt/missing **primary** is re-fetched from a good replica
   (the classic recovery), and a missing/corrupt **replica** is
   rebuilt from a good primary (`rebuilt_replica=true` in the
   result), restoring cross-prefix corruption survivability. It
   verifies the good copy's signature, cross-checks identity, and
   atomic-replaces via `.tmp` + rename; it refuses to overwrite a
   valid copy without `--force`. If neither copy verifies, the
   manifest is unrecoverable and the backup can no longer be
   restored.

4. **Clean up orphans** that may have accumulated from partial
   writes:

   ```sh
   pg_hardstorage repair chunks --orphans --repo <url>            # dry-run
   pg_hardstorage repair chunks --orphans --repo <url> --apply    # delete
   ```

5. **Tombstone unrecoverable backups.** If a backup references a
   missing chunk that has no replica copy, it is unrestorable.
   Tombstone it so it doesn't surface as a restore candidate, and
   put a hold on it for forensic preservation:

   ```sh
   pg_hardstorage hold add <deployment> <backup-id> \
       --holder <oncall> --reason "Corrupt chunks, audit ref <ticket>"
   pg_hardstorage backup delete <deployment> <backup-id>
   ```

6. **Re-run scrub** to confirm clean state:

   ```sh
   pg_hardstorage repair scrub --repo <url>
   ```

## Verification

- `pg_hardstorage repo check <url>` is clean.
- `pg_hardstorage repair scrub <url>` exits 0.
- A test restore of a recent un-affected backup completes and
  passes the `pg_verifybackup` gate (exit 0).
- `pg_hardstorage audit verify-chain` is clean.

## Rollback

There is no rollback for corrupt-at-rest data. If you accidentally
deleted a chunk that was actually fine, `repair chunks --missing`
will surface it the next sweep. Restore the chunk from a backup of
your repo (yes, repos themselves should have backups for tier-0
deployments) or from the replica region.

## Post-incident

- Append an audit event documenting affected backup IDs, chunk
  hashes, and the recovery actions taken.
- If the corruption was widespread, configure or exercise
  cross-region replication: `repo.replicate_to:` in
  `pg_hardstorage.yaml`.
- Schedule scrub more aggressively. Default v0.5+ scrub re-hashes
  N% of chunks per day; tighten N if your storage backend has had
  multiple bit-rot events.
- File a backend-side ticket: object stores generally promise
  durability with explicit guarantees; corruption at rest is a
  reportable event.
