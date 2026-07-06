---
title: Migrate from WAL-G
description: Concept mapping plus the recommended cutover path
              from WAL-G to pg_hardstorage.
tags:
  - migration
  - walg
  - cutover
---

# Migrate from WAL-G

> Move a deployment from WAL-G to pg_hardstorage with the
> same dual-write + retention drain pattern used for
> [pgBackRest migrations](from-pgbackrest.md). No backup
> rewrite required; the old WAL-G repo retires on its own
> retention schedule while pg_hardstorage handles new
> backups.

!!! note "No repo-format conversion — by design"
    WAL-G's repo format is LZ4-compressed PG data files
    plus a JSON sentinel; we have no plans to read it.
    The pattern below treats the migration as an
    **operational cutover**: run both tools side-by-side
    during a retention window, then retire the old repo
    when its window expires.  Old backups stay restorable
    by `wal-g` itself for as long as you keep the binary
    around.

!!! tip "v1.1 ships a drop-in shim"
    `pg_hardstorage` v1.1 ships a `pg-hardstorage-walg`
    binary that mimics the WAL-G CLI surface (5 verbs:
    `backup-push`, `backup-fetch`, `backup-list`,
    `wal-push`, `wal-fetch`).  Symlink it onto PATH as
    `wal-g` and your existing `archive_command` and
    cron jobs keep working but produce native
    pg_hardstorage backups.  See the "v1.1 fast path"
    section below.

## Concept mapping

| WAL-G concept           | pg_hardstorage equivalent          |
|-------------------------|------------------------------------|
| `WALG_S3_PREFIX`        | `--repo s3://…`                    |
| `backup-push`           | `pg_hardstorage backup`            |
| `backup-list`           | `pg_hardstorage list`              |
| `backup-fetch`          | `pg_hardstorage restore`           |
| `wal-push`              | `agent` mode WAL streaming via slot |
| `wal-fetch`             | `wal fetch` for one-off retrieval  |
| Delta backup            | Incremental backup                 |
| `WALG_DELTA_MAX_STEPS`  | Implicit: incrementals roll back to nearest full automatically |
| `WALG_LIBSODIUM_KEY`    | KMS envelope (`encryption.kek_ref` per manifest) |
| `WALG_COMPRESSION_METHOD` | `compression:` config            |

## What you need

- A reachable PostgreSQL cluster currently being backed up
  by WAL-G.
- A target repository URL for pg_hardstorage (different
  bucket / path from WAL-G's repo — keep them separate
  during cutover).
- A KMS endpoint or keyring for the new repo's KEK.

## v1.1 fast path: drop-in shim + config translator

If you'd rather not rewrite cron jobs and `archive_command`
settings, v1.1 lets you keep them — the
`pg-hardstorage-walg` binary parses WAL-G env vars + flags
and dispatches to native pg_hardstorage commands.

```bash
# 1. Install pg_hardstorage (./compile.sh, distro package, or container).

# 2. Translate your existing WAL-G environment in one shot.
#    The translator reads WALG_* env vars from a sourced
#    .env file (or your /etc/default/wal-g) and emits YAML.
pg_hardstorage compat translate --from walg \
    /etc/default/wal-g \
    --output /etc/pg_hardstorage/pg_hardstorage.yaml

# 3. Review the YAML — every unmapped WALG_ setting surfaces
#    as a comment + on stderr.
$EDITOR /etc/pg_hardstorage/pg_hardstorage.yaml

# 4. Initialise the new repo (different from the existing
#    WAL-G prefix).
pg_hardstorage repo init <new-repo-url>

# 5. Build the shim from source (it is not installed by any
#    release artifact) and drop it onto PATH, ahead of
#    /usr/bin/wal-g.
go build -o /usr/local/bin/pg-hardstorage-walg \
    ./cmd/pg-hardstorage-walg
sudo ln -sf /usr/local/bin/pg-hardstorage-walg \
    /usr/local/bin/wal-g

# 6. Verify the symlink wins on PATH.
which wal-g    # → /usr/local/bin/wal-g
wal-g --help   # → "pg-hardstorage-walg mimics..."

# 7. Existing cron job runs unchanged — it now produces a
#    native pg_hardstorage backup.
wal-g backup-push /var/lib/postgresql/15/main
```

Verb coverage in v1.1: `backup-push`, `backup-fetch`,
`backup-list`, `wal-push`, `wal-fetch`.  Less common verbs
(`delete`, `backup-mark`, `catchup-push`, `wal-receive`,
`wal-verify`, `st`, `copy`) exit 2 with a `not implemented
in v1.1; native equivalent: ...` remediation message —
your script sees a clear non-zero exit instead of silent
divergence.

`WALG_DELTA_MAX_STEPS` is honoured implicitly — incrementals
roll back to the nearest full automatically.
`WALG_LIBSODIUM_KEY` is rejected with a pointer to
configuring a KMS envelope (libsodium-box backups stay
readable by `wal-g` itself; new backups land under the
configured KMS).

After the shim is wired, the rest of the cutover (steps 1-5
below) proceeds exactly as documented — the shim simply
replaces the WAL-G binary, not the migration model.

## Steps (full cutover)

### 1. Stand up pg_hardstorage alongside WAL-G

```bash
pg_hardstorage init \
    --pg-connection postgres://pgbackup@db1.example.com/postgres \
    --repo s3://acme-pg-backups-new \
    --yes
```

### 2. Take a fresh full backup

```bash
pg_hardstorage backup db1
```

### 3. Stream WAL via the slot

WAL-G's `wal-push` runs as `archive_command` (or
`archive-async`). pg_hardstorage's WAL streaming uses a
**replication slot**. The two don't interfere — PG
maintains the slot's required WAL retention independently
of `archive_command`.

```bash
sudo systemctl enable --now pg_hardstorage@db1.service
```

`pg_hardstorage doctor` reports the slot's health on the
agent host. The first run after cutover catches up from
the backup's stop_lsn forward.

### 4. Validate restorability

```bash
pg_hardstorage restore db1 latest \
    --target /tmp/restore-test \
    --repo s3://acme-pg-backups-new

pg_hardstorage verify db1 latest --full \
    --repo s3://acme-pg-backups-new
```

### 5. Pick a cutover line and drain WAL-G

Same pattern as [pgBackRest migration](from-pgbackrest.md):

- Choose a cutover line (retention boundary, app window,
  compliance date).
- After the line, stop WAL-G's `archive_command` and
  scheduled `backup-push`.
- Let WAL-G's `delete` runs drain the old repo to its
  retention floor.
- Archive or shut down WAL-G once compliance retention is
  satisfied.

## Why dual-write

A WAL-G backup is LZ4-compressed PG data files plus a
JSON sentinel; the on-disk shape and the chunking
algorithm differ from ours. Converting in place would mean
rewriting every chunk through FastCDC, signing new
manifests, and re-encrypting under the new envelope. Real
work; not free.

Dual-write costs nothing extra — pg_hardstorage's WAL
slot consumes from PG independently of WAL-G's
`archive_command`. The transition window covers the WAL-G
retention floor, then WAL-G retires.

## Differences operators notice

- **Backup IDs.** WAL-G's `base_<lsn>` becomes
  `db1.full.<timestamp>` /
  `db1.incremental.<timestamp>`. The naming matches
  pgBackRest's stanza-aware shape rather than WAL-G's
  LSN-only naming, because pg_hardstorage's manifest
  carries the LSN range as a structured field rather than
  encoding it in the ID.
- **Verification.** WAL-G's verify is a list-and-check;
  ours is fast-verify (chunk hash + signature) plus
  optional `--full` (sandbox-replay with
  `pg_verifybackup`). The full path is far stronger; use
  it on a sample of backups per quarter even at very
  large repo sizes.
- **Encryption.** WAL-G's libsodium box gets replaced by
  AES-256-GCM-SIV with a KMS-wrapped DEK per backup. The
  KEK never leaves the KMS provider; chunks are encrypted
  at rest with a per-backup DEK that's wrapped under the
  KEK in the manifest.

## Troubleshooting

### Slot ballooning

Pg's slot keeps WAL until the consumer acks it. If the
agent is unhealthy and not consuming, WAL accumulates.
`pg_hardstorage doctor` reports slot lag; remediation is
either fix the agent or run `pg_hardstorage wal repair`
which advances the slot to a known-good LSN and recovers
the WAL gap from the latest backup forward.

### WAL-G `archive-async` queue fills

Unrelated to pg_hardstorage. `archive-async` writes WAL to
a local spool first; if WAL-G can't drain to the remote,
the spool fills. During cutover, monitor
`/var/lib/pg_hardstorage` and the WAL-G spool separately;
they're independent failure domains.

### Restoring a WAL-G backup post-cutover

Restoring from the **old WAL-G repo** still requires
WAL-G — pg_hardstorage doesn't read WAL-G's format. Keep
WAL-G installed (just stopped) for the duration of the
cutover window; uninstall once the WAL-G repo is fully
retired.

## Next steps

- [Migrate from pgBackRest](from-pgbackrest.md) — the
  pgBackRest equivalent of this page.
- [Migrate from Barman](from-barman.md) — the Barman
  equivalent.
- [WAL-G shim (Zalando)](../kubernetes/walg-shim.md) —
  for Zalando-operator users who want to keep the
  operator surface and swap the backup binary
  underneath in-pod.
- [Operator Guide](../../operations/operator-guide.md) —
  steady-state operation post-cutover.
