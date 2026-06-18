---
title: Getting started — the interactive helper
description: Six operations through a numbered menu — no flags,
              no subcommands.  Backup, stream WAL, inspect,
              verify, and restore from a single prompt-driven
              binary.
tags:
  - tutorial
  - quick-start
---

# Getting started with `pg_hardstorage_simple`

> The fastest way in.  Launch the binary, pick a number, answer
> the prompts.  No flags, no subcommands, no man-page-spelunking —
> the helper covers the six things most operators do most often.

`pg_hardstorage` has a deliberately large CLI surface — fleet
management, KMS rotation, approval workflows, chaos drills, an LLM
helper, a control-plane server.  Great when you need it, a lot to
read when you don't.

`pg_hardstorage_simple` is the kind-interface companion: one
binary, six numbered menu items, an interactive prompt for every
value.  It exists for the operator who walked in yesterday and
just wants to *back this database up*.

---

## What you need

- `pg_hardstorage` ≥ v0.9 installed on `$PATH`
- `pg_hardstorage_simple` next to it (same release, same `make`)
- A reachable PostgreSQL 15+ instance with a connection string
  that includes the REPLICATION attribute on the user
- 200 MB free disk for the sandbox repo

---

## The six operations

| # | What it does |
|---|---|
| 1 | Set up backups for a database I haven't backed up before |
| 2 | Take a backup right now |
| 3 | Start continuous protection (base backup + WAL streaming) |
| 4 | See what's in my repository |
| 5 | Verify a backup is restorable |
| 6 | Restore a backup |

`q` at any menu quits.  Nothing else is on offer; the helper's
whole promise is to stay simple.  For everything else, run
`pg_hardstorage` directly.

---

## The first run

```bash
pg_hardstorage_simple
```

You'll see the numbered menu.  Pick `1`:

```
  pg_hardstorage — quick start

  What would you like to do?

     1. Set up backups for a database I haven't backed up before
     2. Take a backup right now
     3. Start continuous protection (base backup + WAL streaming)
     4. See what's in my repository
     5. Verify a backup is restorable
     6. Restore a backup
     q. quit

  pick a number
  > 1

  → set up a new deployment
```

Four prompts come next, in order:

1. **PostgreSQL connection string?**
   Default: `postgres://postgres@127.0.0.1/postgres`.  The helper
   does a cheap shape-check (rejects non-libpq URIs) but doesn't
   call the database here — connectivity errors show up at first-
   backup time with the full diagnostic.

2. **What should we call this deployment?**
   Default: the dbname portion of the URL.  Only `[A-Za-z0-9_-]`
   allowed.  This becomes the backup-ID prefix and the manifest's
   `deployment` field.

3. **Where should backups go?**
   Default: `file://<state-dir>/repo` — a per-user filesystem path
   that works offline.  S3 / GCS / Azure / SFTP / SCP URLs accepted;
   credentials come from the same env vars the full binary uses
   (`AWS_ACCESS_KEY_ID`, `GOOGLE_APPLICATION_CREDENTIALS`, …).

4. **Encrypt backups with a local KEK?**
   Default yes.  A 32-byte master key lands in the keyring directory
   (path printed); every chunk is AES-256-GCM-encrypted under a
   per-repo DEK wrapped by that KEK.

The helper then summarises:

```
  About to set up:
    deployment: db1
    pg:         postgres://postgres@127.0.0.1/db1
    repo:       file:///var/lib/pg_hardstorage/repo
    encryption: yes

  Proceed? [Y/n]
```

Hit Enter (the capital `Y` is the obvious default) and:

```
  initialising repo...
  generating signing keypair...
  generating KEK...

  ✓ deployment db1 ready
    repo:    file:///var/lib/pg_hardstorage/repo
    config:  /home/you/.config/pg_hardstorage/pg_hardstorage.yaml

  Take a first backup right now? [Y/n]
```

Saying yes hops straight into operation `#2` against the deployment
you just configured — the test that proves the setup actually
works.

---

## Day-to-day: just back this up

Re-run `pg_hardstorage_simple` and pick `2`:

```
  → take a backup
    using deployment "db1"

  About to take a full backup of db1:
    source: postgres://postgres@127.0.0.1/db1
    target: file:///var/lib/pg_hardstorage/repo

  Continue? [Y/n]

  running backup...

  ✓ db1.full.20260512T103145Z.bf8c
    1290 files · 650 unique chunks · 21s

  this backup is restorable to its stop point.
  for point-in-time recovery between backups, pick #3 to stream WAL.
```

The hint at the bottom is the only thing operators reliably miss
on day one: a base backup alone gets you restorability to the
backup's stop point, but **PITR between backups needs continuous
WAL streaming** (operation `#3`).  The helper surfaces this every
time because the trade-off matters enough to mention.

---

## Continuous protection: keep WAL flowing

Pick `3`:

```
  → start WAL streaming
    using deployment "db1"

  About to start WAL streaming for db1.
    This will run in this terminal until you press Ctrl-C.
    Streamed WAL segments land in the repo so subsequent
    restores can recover past the last basebackup.

  Continue? [Y/n]

  $ pg_hardstorage wal stream ...
```

The streamer runs in the foreground, prints structured progress
events, and unwinds cleanly on Ctrl-C.  For unattended setups
(systemd, k8s, cron) install the full `pg_hardstorage` agent — that
service runs WAL streaming as a managed daemon and the simple
helper deliberately doesn't try to replicate that lifecycle.

---

## Look at what's there

Pick `4`:

```
  → inspect repo
  db1   file:///var/lib/pg_hardstorage/repo
    3 backup(s)

    BACKUP ID                                 WHEN     FILES     ENC
    db1.full.20260512T103145Z.bf8c            2h ago   1290      local:default
    db1.full.20260511T103045Z.7e21            1d ago   1290      local:default
    db1.full.20260510T103044Z.aa11            2d ago   1290      local:default
```

Always human-readable, never JSON.  If you need a machine-parseable
shape, `pg_hardstorage list <deployment> -o json` is the full
binary's job.

---

## Make sure it's actually restorable

Pick `5`:

```
  → verify a backup
    using deployment "db1"

  Which backup?

    *1. db1.full.20260512T103145Z.bf8c
        2h ago · 1290 files
     2. db1.full.20260511T103045Z.7e21
        1d ago · 1290 files
     3. db1.full.20260510T103044Z.aa11
        2d ago · 1290 files
     q. quit

  pick a number
  [1]> ⏎

  verifying db1.full.20260512T103145Z.bf8c ...

  ✓ 650 / 650 chunks ok
```

The helper walks every chunk through the CAS read path: hash check
+ AES-GCM authentication tag verification for encrypted backups.
A failure surfaces the first failing chunk's hash and the suggested
`pg_hardstorage repair scrub` invocation — at which point you've
graduated to the full binary anyway.

---

## Pull a backup back

Pick `6`:

```
  → restore a backup
    using deployment "db1"

  Which backup?
    *1. db1.full.20260512T103145Z.bf8c  …

  pick a number
  [1]> ⏎

  Restore into which directory? (will be created if missing)
  [/tmp/pg_hardstorage-restored]> /var/lib/postgresql/restored

  About to restore:
    backup: db1.full.20260512T103145Z.bf8c (1290 files)
    target: /var/lib/postgresql/restored

  Continue? [Y/n]

  running restore...

  ✓ restored 1290 files (650 chunks · 244376576 bytes) in 18s

  to start the restored cluster:
    pg_ctl -D /var/lib/postgresql/restored start
```

If the target directory isn't empty, the helper refuses unless you
literally type `replace` to confirm — defends against the "wait,
which one was the empty one?" tab-completion mistake.

---

## When to leave the helper

The interactive surface is a deliberately narrow slice.  Use the
full `pg_hardstorage` binary when you need any of:

- Cron / systemd / k8s automation (`pg_hardstorage` has flags;
  the helper does not)
- Key rotation, shred, or HSM-backed KMS (`pg_hardstorage kms …`)
- Fleet-wide queries across deployments (`pg_hardstorage fleet …`)
- Compliance / classification / SLO reports
- The control-plane server (`pg_hardstorage server`)
- Anything multi-tenant

Every operation the helper runs prints the underlying
`pg_hardstorage` command it would have invoked, so a curious
operator can graduate one verb at a time.

---

## What persists between runs

The helper writes one small cache at
`<Config>/simple.yaml` so subsequent runs default to your
last-picked deployment / repo / target dir.  Losing it just means
the next prompt asks fresh; the authoritative deployment list still
lives in `pg_hardstorage`'s normal config files.

---

## Next steps

- [Getting started (full CLI)](getting-started.md) — the
  flag-driven equivalent, with the streamer wired up as a
  long-running service
- [Encryption walkthrough](encryption-walkthrough.md) — local
  KEK and AWS KMS variants
- [PITR walkthrough](pitr-tutorial.md) — point-in-time recovery
  using the WAL the helper's #3 streams
