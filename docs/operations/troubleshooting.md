# Troubleshooting

Symptom-keyed diagnoses. Each entry follows the same shape:

- **Symptom** — what you see.
- **What it means** — what the tool is telling you.
- **What to do** — exact commands.
- **Doctor check** — what `pg_hardstorage doctor` would surface.

For full incident playbooks see
[runbooks/](../reference/runbooks/index.md). For the exit-code
contract see
[operator-guide.md#9-output-modes](operator-guide.md#9-output-modes).

---

## Connection refused / timeout

**Symptom.** A `backup`, `wal stream`, or `init` invocation hangs and
then fails with `pg.connect_failed` or `pg.connect_timeout`.

**What it means.** The agent could not establish a libpq connection
to the deployment's `pg_connection` URL within
`DefaultConnectTimeout` (30 s). libpq's own default is infinite —
we explicitly cap at 30 s so a wedged TCP stack doesn't tie up the
process forever.

**What to do.**

```sh
pg_hardstorage doctor db1               # confirms the symptom
psql 'postgres://pgbackup@db1.example.com/postgres' -c 'SELECT 1'
```

Common causes:
- `pg_hba.conf` doesn't allow the agent's source IP on the
  `replication` virtual database (for `wal stream` and `backup`) or
  on `postgres` (for `init`'s probe). Reload PG after editing.
- A firewall or security group is dropping TCP/5432.
- The host name resolves but the listener is on a different port.
  Pass an explicit `?port=...` in the URL.

Override the timeout per-call by adding `?connect_timeout=N` to the
URL.

**Doctor check.** Reachability check fails with the libpq error
text and a `pg.connect_failed` code.

---

## Auth denied (28000 / 28P01)

**Symptom.** `auth.denied` (exit 3). Body carries the libpq error,
typically `password authentication failed for user "pgbackup"` or
`no pg_hba.conf entry for replication connection from host "..."`.

**What it means.** PG returned SQLSTATE `28000`
(`invalid_authorization_specification`) or `28P01`
(`invalid_password`). The agent maps both to `auth.denied`.

**What to do.**

- For `28P01`: the password is wrong. Check the URL or the
  password store; rotate the role's password if necessary
  (`ALTER ROLE pgbackup WITH PASSWORD '...'`).
- For `28000`: the role lacks `REPLICATION` or `pg_hba.conf` doesn't
  allow this source IP for the `replication` database. Run on PG:

  ```sql
  SELECT rolname, rolreplication FROM pg_roles WHERE rolname = 'pgbackup';
  -- expect rolreplication = t
  ```

  Add an entry to `pg_hba.conf`:

  ```
  host replication pgbackup <agent-ip>/32 scram-sha-256
  ```

  Reload (`SELECT pg_reload_conf()`).

**Doctor check.** Reachability check fails with `auth.denied` code.

---

## Replication slot missing

**Symptom.** `wal stream` exits with `wal.slot_missing`.

**What it means.** The persistent slot
`pg_hardstorage_<deployment>` doesn't exist on the connected PG
node. Most common causes: an admin dropped it, you connected to a
different cluster (different `system_identifier`), or a Patroni
failover happened and the slot didn't get recreated on the new
leader (Strategy A wasn't configured).

**What to do.**

```sh
pg_hardstorage wal repair db1
# or, identical:
pg_hardstorage repair slot db1
```

`wal repair` recreates the slot at PG's current LSN. If the agent's
last confirmed LSN is behind the new slot's `restart_lsn`, the gap
is reported as `wal_gap_detected` and recorded against the repo's
WAL inventory. PITR inside the gap window is then explicitly refused.

**Doctor check.** `slot health` fails with `wal.slot_missing`.

---

## WAL gap detected

**Symptom.** A `wal_gap_detected` notice in `wal stream` logs, or a
restore returns `wal.gap_in_target` (exit 6) when the requested LSN
falls inside a known gap.

**What it means.** Some range of WAL LSNs is missing from the repo.
The gap auditor records the start and end of every gap it knows
about; restores intersecting that range cannot be honoured because
PG would have nothing to replay.

**What to do.**

- For a recent gap caused by `wal repair`: the gap is real WAL loss.
  Pick a restore target outside the gap. The gap bounds are reported
  in the manifest of any backup taken after the gap, and in
  `pg_hardstorage wal list <deployment>`.
- For a gap caused by Patroni failover without `permanent_slots`:
  configure Strategy A (Patroni `permanent_slots`, lands in v0.5;
  today the wizard prints the YAML to add).
- For a gap caused by the agent being killed for too long: take a
  fresh backup. New backups are independent of WAL gaps that
  predate them.

**Doctor check.** `WAL inventory` lists each gap with start/end LSN
and span in seconds.

---

## Repository unreachable

**Symptom.** Any command that touches the repo fails with
`storage.unreachable` (exit 8).

**What it means.** The storage backend returned a network error,
DNS failure, or a 5xx with no recovery. For S3, this might be an
expired credential, a missing IAM role, or a regional outage.

**What to do.**

```sh
pg_hardstorage repo check <repo-url>      # one-shot reachability test
pg_hardstorage doctor                     # validates every configured deployment's repo
```

For S3:
- Verify the credential chain: `aws sts get-caller-identity` from
  the same shell.
- Inspect endpoint and `path_style`: MinIO and other S3-compatible
  stores almost always need `?endpoint=https://...&path_style=true`.
- Check the bucket region matches the URL's `?region=` parameter.

For filesystem repos:
- Permissions on the parent directory.
- The mount point is actually mounted (tools like NFS/EFS can drop).

**Doctor check.** Repo reachability fails with the underlying error.

---

## KMS key unreachable

**Symptom.** `restore` or `verify` exits with
`kms.unreachable` / `kms.key_missing` (exit 8). The body carries the
manifest's `KEKRef` so you know which key the read path was looking
for.

**What it means.** The local keyring file referenced by the
manifest's `KEKRef` is missing, has wrong permissions, or has been
moved. The DEK in the manifest cannot be unwrapped without the
matching KEK.

**What to do.**

```sh
pg_hardstorage kms inspect            # lists keyring contents, modes, mtime
ls -l ~/.config/pg_hardstorage/keyring/
```

If the keyring file is on the wrong host, copy it back. The
`kms inspect` output includes a SHA-256 fingerprint of the public
signing key — use that to match the right keyring to the right
backup set.

If the key is genuinely lost: backups encrypted under it are
unrecoverable. This is the crypto-shred semantic working as designed;
plan key custody before you need it.

**Doctor check.** `keystore presence` reports each expected file.

---

## Verify failed (exit 9)

**Symptom.** `verify` or `repair scrub` exits 9 with a body listing
the failing chunk(s).

**What it means.** The decrypted plaintext SHA-256 of one or more
chunks does not match the value the manifest declares. Either the
manifest was tampered with, the chunk on disk is corrupt, or there
was an envelope-format mismatch (very rare).

**What to do.**

```sh
pg_hardstorage repo check <repo-url>          # narrows to the failing manifest
pg_hardstorage repair chunks --missing        # if chunks have actually disappeared
pg_hardstorage repair chunks --orphans        # safe to delete unreferenced chunks
```

If the corrupt chunk is referenced by a backup that has a replica
region, fetch from there. v0.5 will auto-heal from the replica;
today this is manual.

If the failing manifest is the primary copy, repair from the replica:

```sh
pg_hardstorage repair manifest db1 <backup-id>
```

**Doctor check.** Not surfaced; verify is its own command.

---

## Pre-flight refusal (exit 4)

**Symptom.** Any destructive command (`restore`, `rotate --apply`,
`repair X --apply`, `repo gc --apply`) exits 4 without touching
state.

**What it means.** A pre-flight check identified a condition that
makes the operation unsafe. No bytes have moved.

**What to do.** Read the body. Each refusal carries a `Suggestion`
with the exact next step. Common refusals:

- Restore target dir non-empty → use `--force` after confirming.
- Restore target dir contains live `postmaster.pid` → don't restore
  on top of a running PG.
- Restore on a Patroni primary → pick a replica node; the suggestion
  tells you which.
- KMS key unreachable → see above.
- Manifest signature verification failed → `repair manifest` or
  `repair attestation` depending on which side is broken.

**Doctor check.** Doctor doesn't surface these (they're per-call).

---

## Patroni split-brain refusal

**Symptom.** `wal stream` or `backup` exits 4 with
`preflight.patroni_split_brain`.

**What it means.** The agent's view of "who is the leader" disagrees
with PG's view. Either Patroni's REST endpoint reports a different
leader than the connected node thinks it is, or two candidates
report `role=primary`. Continuing would risk writing WAL or backup
chunks tagged with the wrong timeline.

**What to do.** This is an operator-only resolution. See
[runbooks/R7-patroni-split-brain](../reference/runbooks/R7-patroni-split-brain.md).
Do not `--force` past it.

**Doctor check.** `Patroni topology` flags it as a critical finding.

---

## Restore target not empty

**Symptom.** `restore` exits 4 with `preflight.target_not_empty`.

**What it means.** The `--target` directory is not empty. The
restore won't overwrite arbitrary files.

**What to do.**

- For a clean restore: pick an empty target directory.
- For a deliberate overwrite: pass `--force`. The body lists every
  top-level entry that will be deleted, and the operation prompts
  again before doing anything. With `--force --confirm` the second
  prompt is suppressed.

`--force` is destructive: it removes the existing contents of the
target directory before unpacking. Use it on data dirs, not on `/`.

**Doctor check.** N/A.

---

## Crash mid-backup

**Symptom.** Agent killed (`SIGKILL`, OOM, host reboot) during a
backup. On restart, `pg_hardstorage doctor` reports a stale inflight
record; `pg_backup_start` may still be open on PG.

**What it means.** Idempotent design: the chunks already uploaded
are CAS-keyed and safe; the `pg_backup_start` slot needs releasing.

**What to do.** The agent reconciles `state/inflight.json` on
startup automatically — it issues `pg_backup_stop(false)` to release
server-side state and marks the manifest aborted (never committed).
You don't normally need to do anything.

If the agent isn't running and you want manual cleanup:

```sql
SELECT pg_backup_stop(false);   -- on the affected PG, as a superuser
```

This releases the backup label without waiting for archive. The
partial manifest in the repo is never visible (it lived as `.tmp`
and never got renamed).

**Doctor check.** `inflight reconciliation` reports any stale record
and the action it took.

---

## Disk full mid-backup

**Symptom.** `backup` exits with `storage.no_space` (exit 8) or the
filesystem returns ENOSPC during chunk writes.

**What it means.** The repository ran out of space. The pre-flight
capacity check (asserts repo has at least 110% of projected backup
size free) is configurable but not infallible — a parallel writer
(another deployment, another tenant) can eat space between
pre-flight and commit.

**What to do.**

```sh
pg_hardstorage repo usage <repo-url>      # bytes-by-category
pg_hardstorage repo gc <repo-url>         # dry-run reclaim
pg_hardstorage repo gc <repo-url> --apply # actually reclaim orphans
```

If GC alone doesn't free enough, run `rotate --apply` to tombstone
old backups, then `repo gc --apply` to actually delete the chunks.
Tombstoned + GC'd chunks are gone permanently; this is destructive.

**Doctor check.** `Repository capacity` reports free / projected
ratio.

---

## Slow uploads / saturation

**Symptom.** Backup throughput is below what the network and storage
can sustain. NDJSON progress events show
`chunker_paused reason=backpressure stage=storage_put`.

**What it means.** The chunker is faster than the storage backend.
Backpressure is intentional — we never buffer-bomb memory to mask
slow storage. The agent's adaptive concurrency starts at 4 parallel
uploads and ramps based on observed RTT and error rate (TCP-style).

**What to do.**

- Confirm the backend bandwidth: a sequential `aws s3 cp` of a
  large object from the same host gives the actual ceiling.
- For S3 endpoints with low per-connection bandwidth (some
  endpoints regional throttle), set
  `max_storage_concurrency: 32` in the deployment config.
- For bandwidth-sensitive environments, set a hard cap with
  `max_storage_mb_per_second: 100` so the chunker doesn't burn CPU
  it can't ship.
- Conversely, if the host is saturating egress and that's hurting
  user queries, lower the cap rather than letting backpressure
  oscillate.

**Doctor check.** N/A; throughput tuning is per-deployment.
