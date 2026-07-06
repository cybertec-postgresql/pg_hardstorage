---
title: Migrate from Barman
description: Concept mapping plus the recommended cutover path
              from Barman to pg_hardstorage.
tags:
  - migration
  - barman
  - cutover
---

# Migrate from Barman

> Move a deployment from Barman to pg_hardstorage with the
> same dual-write + retention drain pattern used for the
> [pgBackRest](from-pgbackrest.md) and
> [WAL-G](from-walg.md) migrations. No backup rewrite
> required.

!!! note "No repo-format conversion — by design"
    Barman's repo format is binary-tagged and undocumented
    externally; we have no plans to read it.  The pattern
    below is **operational cutover** — keep Barman running
    until its retention window expires; new backups land
    in a fresh pg_hardstorage repo.

!!! tip "v1.1 ships a drop-in shim"
    `pg_hardstorage` v1.1 ships `pg-hardstorage-barman` and
    `pg-hardstorage-barman-wal-archive` binaries that mimic
    the Barman CLI surface.  Symlink them onto PATH and
    your existing cron jobs + `archive_command` settings
    keep working but produce native pg_hardstorage backups.
    See the "v1.1 fast path" section below.

## Concept mapping

| Barman concept           | pg_hardstorage equivalent          |
|--------------------------|------------------------------------|
| Server (`/etc/barman.d/db1.conf`) | Deployment (`db1`)        |
| `barman backup`          | `pg_hardstorage backup`            |
| `barman list-backup`     | `pg_hardstorage list`              |
| `barman recover`         | `pg_hardstorage restore`           |
| `archive_command`        | WAL streaming via slot (no `archive_command` change) |
| Streaming-only mode      | Default: agent uses replication slot |
| `barman check`           | `pg_hardstorage doctor`            |
| Retention policy         | `retention:` in deployment config  |
| `barman switch-wal`      | Inferred — backup commit triggers WAL switch automatically |
| `barman-cloud-backup`    | Cloud storage backend (`s3://`, `gs://`, `azure://`) |

## What you need

- A reachable PostgreSQL cluster currently being backed up
  by Barman.
- A target repository URL for pg_hardstorage. If you're
  using `barman-cloud-*`, pick a different bucket / prefix
  to keep the two systems separate during cutover.
- A KMS endpoint or keyring for the new repo's KEK.

## v1.1 fast path: drop-in shim + config translator

```bash
# 1. Install pg_hardstorage (./compile.sh, distro package, or container).

# 2. Translate your existing barman.conf in one shot.
pg_hardstorage compat translate --from barman \
    /etc/barman.conf \
    --output /etc/pg_hardstorage/pg_hardstorage.yaml

# 3. Review the YAML — every unmapped Barman setting
#    surfaces as a comment + on stderr.  Multi-server
#    barman.conf with [server] sections produces multiple
#    deployment entries.
$EDITOR /etc/pg_hardstorage/pg_hardstorage.yaml

# 4. Initialise the new repo.
pg_hardstorage repo init <new-repo-url>

# 5. Build the shim binaries from source (they are not
#    installed by any release artifact) and drop them on PATH.
go build -o /usr/local/bin/pg-hardstorage-barman \
    ./cmd/pg-hardstorage-barman
go build -o /usr/local/bin/pg-hardstorage-barman-wal-archive \
    ./cmd/pg-hardstorage-barman-wal-archive
sudo ln -sf /usr/local/bin/pg-hardstorage-barman \
    /usr/local/bin/barman
sudo ln -sf /usr/local/bin/pg-hardstorage-barman-wal-archive \
    /usr/local/bin/barman-wal-archive

# 6. Existing cron + archive_command run unchanged.
barman backup db1
barman list-backup db1
barman recover db1 db1.full.<id> /var/lib/postgresql/restored
```

Verb coverage in v1.1: `backup`, `recover`, `list-backup`,
`show-backup`, `check`, `delete`, plus the dedicated
`barman-wal-archive` binary for `archive_command` use.
15 less-common verbs (`cron`, `archive-wal`, `switch-wal`,
`diagnose`, `verify`, `keep`, `receive-wal`, `replication-status`,
`show-server`, `list-server`, `lock-directory-cleanup`,
`rebuild-xlogdb`, `get-wal`, `put-wal`, etc.) refuse with
exit 2 + a remediation pointing at the native equivalent.

`--target-xid` refuses (we don't have a transaction-id
lookup; use `--target-time` or native `--to-lsn` instead).
`--remote-ssh-command`, `--get-wal/--no-get-wal`, and
`--retry-times` silently drop — native uses
replication-protocol over libpq (no SSH) with built-in
exponential backoff.

After the shim is wired, the rest of the cutover (steps 1-8
below) proceeds exactly as documented — the shim replaces
the Barman binaries, not the migration model.

## Steps (full cutover)

### 1. Stand up pg_hardstorage alongside Barman

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

### 3. Stream WAL via the agent's slot

Barman's WAL receiver and `pg_hardstorage`'s agent both
use `pg_receivewal`-style streaming under the hood. Each
maintains its own replication slot, so they coexist
without conflict.

```bash
sudo systemctl enable --now pg_hardstorage@db1.service
```

If your Barman setup uses `archive_command` rather than
streaming-only mode, leave it alone — pg_hardstorage's
slot path doesn't compete with `archive_command`.

### 4. Validate restorability

```bash
pg_hardstorage restore db1 latest \
    --target /tmp/restore-test \
    --repo s3://acme-pg-backups-new

pg_hardstorage verify db1 latest --full \
    --repo s3://acme-pg-backups-new
```

### 5. Pick a cutover line and drain Barman

After the cutover line:

- Stop Barman's scheduled backups (`barman cron` /
  systemd timer).
- Let Barman's retention runs drain the old repo to
  its retention floor.
- Archive the old repo and decommission the Barman host
  once compliance retention is satisfied.

## Things Barman users will recognise (and where we differ)

### Streaming-first

Barman's recommended posture is streaming-only with a
replication slot per server; ours is the same. Operators
moving from Barman streaming-only feel at home — slot
naming, ack semantics, `pg_hardstorage doctor`'s slot
section all map cleanly to Barman's `barman check`
output.

### Backup catalogue is separate from the data

Barman maintains backup metadata in `~barman/<server>/base/<id>/`;
ours lives in `manifests/<deployment>/backups/<id>/` on the
**same** repo as the chunks. Operators with strict
catalogue-vs-data separation requirements (a common Barman
pattern with NFS catalogue + S3 data) need to keep that in
mind: the new model puts metadata and chunks together for
the integrity story (signed manifest sits next to the
chunks it references).

### `barman recover` vs `pg_hardstorage restore`

Barman's recover supports `--remote-ssh-command` to
recover onto a remote host directly. Our restore is
local-target by design (run the restore where the target
PG will run). For remote restores, run the agent on the
target host and use `pg_hardstorage restore` from there.

### `barman-cli` companion package

We don't ship a separate companion package. The agent
runs on the PG host (or in a sidecar pod); the CLI runs
anywhere a binary can. A "barman-cli equivalent" is just
`pg_hardstorage` itself — same binary, different
subcommand.

## Why dual-write

The Barman backup catalogue isn't readable by us, and a
one-shot importer would mean re-chunking every backup
through FastCDC and re-signing manifests. Real work,
proportional to repo size.

Dual-write costs nothing extra — pg_hardstorage's slot
streams from PG independently of Barman's slot. The
transition window covers Barman's retention floor, then
Barman retires.

## Troubleshooting

### Two slots, two consumers

Both pg_hardstorage and Barman maintain their own
replication slot during cutover. That's intentional —
neither tool acks WAL the other needs. Monitor with:

```sql
SELECT slot_name, active, pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn)
FROM pg_replication_slots;
```

If either slot lags significantly, investigate the
unhealthy consumer first. `pg_hardstorage doctor` covers
our slot; `barman check` covers Barman's.

### Restoring a Barman backup post-cutover

Restoring from the old Barman repo still requires Barman
— we don't read Barman's format. Keep Barman installed
(just stopped) for the duration of the cutover window;
decommission once the Barman repo is fully retired.

### Recovery to a remote host

Run the pg_hardstorage agent on the target host and
restore from there. The repo URL is reachable from
anywhere with network + auth; the restore writes locally.

## Next steps

- [Migrate from pgBackRest](from-pgbackrest.md).
- [Migrate from WAL-G](from-walg.md).
- [Operator Guide](../../operations/operator-guide.md) —
  steady-state operation post-cutover.
- [Air-gapped operation](../air-gapped/enable-policy.md) —
  for Barman users running in regulated networks where
  the new air-gap posture is a relevant differentiator.
