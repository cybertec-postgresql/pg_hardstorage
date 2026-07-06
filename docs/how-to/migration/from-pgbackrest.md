---
title: Migrate from pgBackRest
description: Concept mapping plus the recommended cutover path
              from a pgBackRest deployment to pg_hardstorage.
tags:
  - migration
  - pgbackrest
  - cutover
---

# Migrate from pgBackRest

> Move a deployment from pgBackRest to pg_hardstorage with
> minimum risk. The recommended cutover is **dual-write +
> retention drain**: keep the old repo for the legacy
> retention window, write all new backups into the new
> repo, switch restores at a chosen cutover line. No
> backup rewrite required.

!!! note "No repo-format conversion — by design"
    pgBackRest's repo format is binary-tagged and
    undocumented externally; we have no plans to read it.
    The pattern below treats the migration as an
    **operational cutover**: run both tools side-by-side
    during a retention window, then retire the old repo
    when its window expires.  Old backups stay restorable
    by `pgbackrest` itself for as long as you keep the
    binary around.

!!! tip "v1.1 ships a drop-in shim"
    `pg_hardstorage` v1.1 ships a `pg-hardstorage-pgbackrest`
    binary that mimics the pgBackRest CLI surface.  Symlink
    it onto PATH as `pgbackrest` and your existing cron
    jobs + `archive_command` settings keep working but
    produce native pg_hardstorage backups.  See the
    "v1.1 fast path" section below.

## Concept mapping

| pgBackRest concept       | pg_hardstorage equivalent           |
|--------------------------|-------------------------------------|
| Stanza                   | Deployment (`db1`, `db2`, ...)      |
| Repo (`repo1-path`)      | Repository URL (`s3://…`, `file://…`) |
| Full backup              | Full backup (`db1.full.<ts>`)       |
| Differential / incremental | Incremental backup (incremental of nearest full) |
| `archive-push` /
  `archive-get`            | WAL streaming via `pg_hardstorage agent` |
| `restore`                | `pg_hardstorage restore`            |
| `info`                   | `pg_hardstorage list` + `show`      |
| `expire`                 | `pg_hardstorage rotate`             |
| Cipher (`repo1-cipher-pass`) | KMS envelope (`encryption.kek_ref` per manifest) |
| Repo verify (`pgbackrest verify`) | `pg_hardstorage verify` (fast or `--full`) |

The conceptual model is similar — the bytes-on-disk shape
is not.

## What you need

- A reachable PostgreSQL cluster currently being backed up
  by pgBackRest.
- A target repository URL for pg_hardstorage (different
  bucket / path from pgBackRest's repo — keep them
  separate during cutover).
- A KMS endpoint or local keyring for the new repo's KEK.

## v1.1 fast path: drop-in shim + config translator

If you'd rather not rewrite cron jobs and `archive_command`
settings, v1.1 lets you keep them — the
`pg-hardstorage-pgbackrest` binary parses pgBackRest flags
and dispatches to native pg_hardstorage commands.

```bash
# 1. Install pg_hardstorage (./compile.sh, distro package, or container).

# 2. Translate your existing pgbackrest.conf in one shot.
pg_hardstorage compat translate --from pgbackrest \
    /etc/pgbackrest/pgbackrest.conf \
    --output /etc/pg_hardstorage/pg_hardstorage.yaml

# 3. Review the YAML — every unmapped pgBackRest setting
#    surfaces as a comment + on stderr.
$EDITOR /etc/pg_hardstorage/pg_hardstorage.yaml

# 4. Initialise the new repo (different from the existing
#    pgbackrest repo).
pg_hardstorage repo init <new-repo-url>

# 5. Build the shim from source (it is not installed by any
#    release artifact) and drop it onto PATH, ahead of
#    /usr/bin/pgbackrest.
go build -o /usr/local/bin/pg-hardstorage-pgbackrest \
    ./cmd/pg-hardstorage-pgbackrest
sudo ln -sf /usr/local/bin/pg-hardstorage-pgbackrest \
    /usr/local/bin/pgbackrest

# 6. Verify the symlink wins on PATH.
which pgbackrest    # → /usr/local/bin/pgbackrest
pgbackrest --help   # → "pg-hardstorage-pgbackrest mimics..."

# 7. Existing cron job runs unchanged — it now produces a
#    native pg_hardstorage backup.
pgbackrest --stanza=db1 backup
```

Verb coverage in v1.1: `stanza-create`, `backup`,
`restore`, `archive-push`, `archive-get`, `info`, `check`,
`verify`.  Anything else exits 2 with a `not implemented in
v1.1; native equivalent: ...` remediation message — your
script sees a clear non-zero exit instead of silent
divergence.

`--type=diff` refuses with a pointer to `--incremental-from`
(PG 17 page-level incremental — the modern equivalent).
`--archive-async` and friends silently drop because native
streaming is already async via the replication slot.

After the shim is wired, the rest of the cutover (steps 1-8
below) proceeds exactly as documented — the shim simply
replaces the pgBackRest binary, not the migration model.

## Steps (full cutover)

### 1. Stand up pg_hardstorage alongside pgBackRest

Install the binary, create a deployment, point it at a new
repo. The two systems coexist — pgBackRest keeps doing its
thing on the old repo, pg_hardstorage starts doing its
thing on the new one.

```bash
pg_hardstorage init \
    --pg-connection postgres://pgbackup@db1.example.com/postgres \
    --repo s3://acme-pg-backups-new \
    --yes
```

This creates `/etc/pg_hardstorage/pg_hardstorage.yaml` with
a deployment named after the host (override with
`--deployment db1`).

### 2. Take a fresh full backup

```bash
pg_hardstorage backup db1
```

The new repo now holds a full backup that pg_hardstorage
controls end-to-end (chunks + manifest + signed
attestation). Subsequent runs default to incremental.

### 3. Configure dual-write WAL

Run pg_hardstorage's WAL streaming agent in parallel with
pgBackRest's `archive_command`. Both consume from PG via
their own primitives:

- pgBackRest reads via `archive_command` (an
  `archive-push` per WAL segment).
- pg_hardstorage's agent streams WAL via a replication
  slot (`pg_hardstorage_db1`).

Both work in parallel without conflict. PG doesn't notice.

```bash
sudo systemctl enable --now pg_hardstorage@db1.service
```

### 4. Validate restorability

Before draining pgBackRest, confirm pg_hardstorage backups
restore cleanly:

```bash
pg_hardstorage restore db1 latest \
    --target /tmp/restore-test \
    --repo s3://acme-pg-backups-new

pg_hardstorage verify db1 latest --full \
    --repo s3://acme-pg-backups-new
```

### 5. Pick a cutover line

The cutover line is "the last LSN you want to be able to
restore to via pgBackRest." After this LSN, you restore
through pg_hardstorage; before this LSN, you restore
through whichever tool covers it.

Common choices:

- A natural retention boundary (the next monthly).
- An app maintenance window where the old repo gets
  retired.
- A regulatory date — once compliance retention is
  satisfied, pgBackRest can stop entirely.

### 6. Drain pgBackRest (operator-paced)

After the cutover line:

- Stop pgBackRest's `archive_command` and scheduled
  backups.
- Let pgBackRest's `expire` runs drain the old repo to
  its retention floor.
- Once compliance retention is satisfied, archive the old
  repo's bytes (cold-storage S3, off-premise tape) and
  shut pgBackRest down.

Throughout this period, pg_hardstorage is the **active**
backup tool. PITR within the dual-write window is covered
by either tool; PITR before the dual-write window starts
is covered by pgBackRest only.

## Why dual-write rather than a one-shot import

A pgBackRest backup contains files and a `backup.manifest`
ledger; we'd need to re-chunk every file through FastCDC,
sign new manifests, and rewrite WAL into our format. That
work is real and proportional to the source repo size — for
a 100 TB repo, hours-to-days of CPU.

Dual-write costs nothing extra (the source PG is already
producing WAL; pg_hardstorage just consumes it from the
slot). The trade-off is a longer transition window during
which both tools coexist.

For deployments where retention is short (< 30 days), the
dual-write window is naturally short and the old repo
retires itself.

## Cross-tool restorability

During the cutover window:

- pgBackRest restore against pgBackRest repo: as before.
- pg_hardstorage restore against pg_hardstorage repo:
  see [Operator Guide § Restore](../../operations/operator-guide.md).
- **Cross-tool restore is not supported.** Each tool reads
  only its own repo format.

## Troubleshooting

### Conflicting WAL primaries

Don't run **two** `archive_command`s simultaneously
against the same WAL segments. pg_hardstorage's WAL
streaming uses a replication slot, **not**
`archive_command`, so the two stay out of each other's way
by design. If you previously had pgBackRest configured for
both `archive_command` and `archive-async`, leave
`archive_command` alone — pg_hardstorage doesn't compete
with it.

### Slot lag while pgBackRest still owns archiving

The agent's slot acks WAL only after streaming. With
pgBackRest's `archive_command` also consuming
(through its own LSN tracker, not the slot), PG keeps
the WAL until both are caught up. That's the desired
posture during cutover; monitor `pg_hardstorage doctor`
for slot lag and tune `wal_keep_size` accordingly.

### Old repo's KEK is gone

If pgBackRest's `repo1-cipher-pass` is lost, you can
neither restore nor verify the old repo. pg_hardstorage's
posture (KMS-managed KEK with `kms verify` round-trip
on every startup) avoids this — confirm the new
deployment's KEK is reachable via:

```bash
pg_hardstorage kms verify
```

before cutover.

## Next steps

- [Migrate from WAL-G](from-walg.md) — the WAL-G
  equivalent of this page.
- [Migrate from Barman](from-barman.md) — the Barman
  equivalent.
- [Operator Guide](../../operations/operator-guide.md) —
  what steady-state operation looks like once cutover is
  done.
- [pgBackRest shim](../kubernetes/pgbackrest-shim.md) —
  for Crunchy PGO users who want to keep PGO and swap the
  backup binary underneath (v0.5).
