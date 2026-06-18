---
title: First backup and restore
description: Take a full backup against a sandbox PostgreSQL and restore it
              into a fresh data dir, gated by pg_verifybackup.
tags:
  - backup
  - restore
  - tutorial
---

# First backup and restore

> Backs up a real PostgreSQL deployment, restores it into a sandbox
> data dir on the same host, and confirms restorability with
> `pg_verifybackup`. About 10 minutes against a small database; the
> commands scale unchanged to a 100 TB cluster.

This tutorial walks the round-trip every operator should run before
trusting any backup tool — *including* this one. You finish with a
restored data directory you can `pg_ctl start` against, and the
verifier's stamp on the manifest.

This tutorial deliberately covers **just the base-backup path** —
no continuous WAL.  The base backup is one of two pieces; in
production you also run `pg_hardstorage wal stream` 24/7 so that
PITR can roll forward between backups.  See the
[PITR walkthrough](pitr-tutorial.md) for the full picture.

If you have not installed yet, do
[Getting started](getting-started.md) first.

---

## What you need

- A reachable PostgreSQL 15+ instance.  The `postgres` superuser will
  do for the tutorial; in production you would use a dedicated
  replication role (see [getting-started](getting-started.md#21-provision-a-replication-user-on-postgresql)).
- 2 GB free disk for the sandbox repo + restored data dir.
- `pg_hardstorage` v0.2 or later on `$PATH`.
- `pg_verifybackup` from the matching PG client tools (ships with
  `postgresql-client-17` etc.).

A throwaway PostgreSQL in Docker works for the tutorial:

```bash
docker run -d --name hs-tutorial-pg \
    -e POSTGRES_PASSWORD=postgres \
    -p 5432:5432 \
    postgres:17
```

Add some data so the round-trip proves something:

```bash
PGPASSWORD=postgres psql -h 127.0.0.1 -U postgres -c \
    "CREATE TABLE hello (id int PRIMARY KEY, msg text);
     INSERT INTO hello VALUES (1,'world'),(2,'restore-me');"
```

---

## Steps

### 1. Create a sandbox repository

```bash
# RUNNABLE
pg_hardstorage repo init file:///tmp/hs-tutorial-repo
```

The repo is just a directory: `chunks/`, `manifests/`, `wal/`,
`audit/`, plus a top-level `HSREPO` magic file.  Re-running against an
existing repo returns `conflict.repo_exists` (exit 7) — the operation
is idempotent on the URL.

### 2. Take a full backup

```bash
# RUNNABLE
pg_hardstorage backup db1 \
    --pg-connection "${PG_CONNECTION:-postgres://postgres:postgres@127.0.0.1/postgres}" \
    --repo file:///tmp/hs-tutorial-repo
```

The pipeline is `BASE_BACKUP` over libpq → tar parser → FastCDC
chunker → CAS PUTs → signed manifest. On the first run a signing
keypair is generated under your keyring directory (run
`pg_hardstorage doctor` to print the exact path).

Sample output:

```console
✓ Connected to PostgreSQL 17.x
✓ Backup db1.full.20260504T120000Z.a1b2 complete · 33 MB physical · 1 chunk
✓ Manifest committed (signed, ed25519)
```

The backup ID has the shape `db1.full.YYYYMMDDThhmmssZ.<hash>` —
UTC, no local-zone surprises at 3am, plus a 4-char hash to keep
sub-second back-to-back backups distinct.

### 3. List what's in the repo

```bash
# RUNNABLE
pg_hardstorage list db1 --repo file:///tmp/hs-tutorial-repo
```

```console
ID                                       STARTED              SIZE      STATE
db1.full.20260504T120000Z.a1b2           2026-05-04 12:00:00  33 MB     committed
```

The backup ID has the shape `db1.full.YYYYMMDDThhmmssZ.<hash>` —
the trailing 4-char hash disambiguates backups taken in the same
second.  Capture the latest one for the next steps:

```bash
# RUNNABLE
BACKUP_ID=$(pg_hardstorage list db1 --repo file:///tmp/hs-tutorial-repo -o json \
    | grep -oE 'db1\.full\.[0-9TZ]+\.[0-9a-f]+' | head -1)
echo "BACKUP_ID=$BACKUP_ID"
```

### 4. Inspect the manifest

```bash
# RUNNABLE
pg_hardstorage show db1 "$BACKUP_ID" \
    --repo file:///tmp/hs-tutorial-repo
```

`show` prints LSN range, timeline, dedup ratio, encryption envelope
state, and a verification record once one is written.  Pipe through
`-o json` if you want to parse it.

### 5. Verify the manifest without restoring

```bash
# RUNNABLE
pg_hardstorage verify db1 latest \
    --repo file:///tmp/hs-tutorial-repo
```

`verify` validates the manifest's ed25519 signature with your local
public key, then SHA-256-round-trips every referenced chunk through
the CAS read path. Encrypted backups are decrypted in-process. No
data dir is materialised.

For a much faster pre-flight that only checks chunk presence (no
fetch, no SHA), pass `--existence-only`. Useful before
`backup undelete` to confirm chunk-GC has not yet reaped the bytes.

### 6. Restore to a sandbox directory

```bash
# RUNNABLE skip-in-ci="postverify pg_ctl-start needs continuous WAL in the repo; this tutorial deliberately omits `wal stream`, so recovery hangs reading trailing segments. See pitr-tutorial.md."
pg_hardstorage restore db1 latest \
    --repo file:///tmp/hs-tutorial-repo \
    --target /tmp/hs-tutorial-restored
```

Sample output:

```console
✓ Pre-flight: repo reachable, KMS reachable, target empty
✓ Restored 1 chunk · 33 MB to /tmp/hs-tutorial-restored
✓ pg_verifybackup OK
```

The post-restore `pg_verifybackup` is the gate that decides exit
code: 0 on success, 9 if the verifier rejects anything. `--verify=skip`
turns it off (audited; do not skip in production).

### 7. Boot the restored data dir

```bash
docker run --rm -d --name hs-tutorial-restored \
    -v /tmp/hs-tutorial-restored:/var/lib/postgresql/data \
    -p 5433:5432 \
    -e POSTGRES_PASSWORD=postgres \
    postgres:17
```

```bash
PGPASSWORD=postgres psql -h 127.0.0.1 -p 5433 -U postgres \
    -c "SELECT * FROM hello;"
```

```console
 id |    msg
----+------------
  1 | world
  2 | restore-me
(2 rows)
```

Your row survived the round-trip.

### 8. Tear down

```bash
docker rm -f hs-tutorial-pg hs-tutorial-restored
rm -rf /tmp/hs-tutorial-repo /tmp/hs-tutorial-restored
```

---

## What just happened

You exercised the full data plane: a base backup over the replication
protocol, content-addressed chunk storage with a signed manifest, an
independent verify step that re-hashes every chunk, and a restore that
rebuilds the data directory and confirms it with the upstream
`pg_verifybackup` tool.

The repo on disk is the source of truth: rerun `pg_hardstorage list`
or `pg_hardstorage show` from another machine pointing at the same
URL and you get the same answers. Local agent state is regenerable —
deleting the cache is harmless.

---

## Next steps

- [PITR walkthrough](pitr-tutorial.md) — replay WAL up to a
  natural-language timestamp.
- [Encryption walkthrough](encryption-walkthrough.md) — wrap chunks
  with a local KEK or AWS KMS.
- [Operator guide — daily operations](../operations/operator-guide.md) —
  what `status`, `list`, and `show` tell you in production.
- [R3 — Cold start from backups](../reference/runbooks/R3-cold-start-from-backups.md) —
  what to do when the source PG is gone, only the repo remains.
