# Getting Started

`pg_hardstorage` is a PostgreSQL backup tool built around
**continuous WAL streaming over the replication protocol**.  In
production you run two processes side by side: `wal stream`
continuously receives WAL from PG and commits every completed
16 MiB segment into the repo, and `backup` lays down a base
backup on a schedule (e.g. nightly) that the stream rolls
forward from.  Daily backup + always-on stream = PITR to any
segment-aligned point.

It backs up PostgreSQL you run yourself — bare metal, VMs, containers,
Patroni clusters, and operators like CloudNativePG — over the physical
replication protocol, deduplicates and encrypts content-addressed
chunks, and restores with PITR.  PG 15+, Apache 2.0.

Managed DBaaS (Amazon RDS, Aurora, Cloud SQL, Azure Database, Neon,
Supabase, and similar) do **not** expose the `BASE_BACKUP` replication
command to customers, so pg_hardstorage cannot take a physical base
backup of them — see the [FAQ](../faq.md).

This page gets you from zero to a running streamer, a first base
backup, and a restored data dir in five minutes. After that, see
the [operator guide](../operations/operator-guide.md).

---

## 1. Install

### Pre-built binary

Releases ship as static `linux/{amd64,arm64}` and `darwin/arm64`
tarballs (Windows is CLI-only). Grab the matching one from
[github.com/cybertec-postgresql/pg_hardstorage/releases](https://github.com/cybertec-postgresql/pg_hardstorage/releases),
verify the cosign signature, and drop the binary on your `$PATH`:

```sh
VERSION=1.0.11   # latest release: https://github.com/cybertec-postgresql/pg_hardstorage/releases/latest
curl -LO "https://github.com/cybertec-postgresql/pg_hardstorage/releases/download/v${VERSION}/pg_hardstorage_${VERSION}_linux_amd64.tar.gz"
tar xzf "pg_hardstorage_${VERSION}_linux_amd64.tar.gz"
sudo install -m 0755 pg_hardstorage /usr/local/bin/
pg_hardstorage version
```

### `.deb` (Debian / Ubuntu)

```sh
VERSION=1.0.11   # the release you downloaded
sudo dpkg -i "pg-hardstorage_${VERSION}_amd64.deb"
```

The package installs the binary at `/usr/bin/pg_hardstorage`, drops a
systemd unit at `/lib/systemd/system/pg_hardstorage.service`, and
creates `/etc/pg_hardstorage/`, `/var/lib/pg_hardstorage/`,
`/var/log/pg_hardstorage/` with mode 0750 owned by `pg-hardstorage`.

### `.rpm` (Fedora / RHEL / Rocky / Alma)

```sh
VERSION=1.0.11   # the release you downloaded
sudo rpm -i "pg-hardstorage-${VERSION}-1.x86_64.rpm"
```

Same layout as the `.deb`.

### Container image

Pre-built images are not yet published to a registry. Build a
distroless image locally from the in-tree Dockerfile:

```sh
docker build -t pg_hardstorage:local -f deploy/docker/Dockerfile .
docker run --rm pg_hardstorage:local version
```

The image is distroless and runs as `nonroot`. Mount a config dir at
`/etc/pg_hardstorage` and a state dir at `/var/lib/pg_hardstorage`;
both must be writable by UID 65532.

### From source

```sh
git clone https://github.com/cybertec-postgresql/pg_hardstorage
cd pg_hardstorage
make build                 # produces bin/pg_hardstorage (bare `make` prints the help menu)
sudo install -m 0755 bin/pg_hardstorage /usr/local/bin/
```

Requires Go 1.26+. `make test` runs the full unit suite under the race
detector; `make test-integration` exercises a real PostgreSQL
container via testcontainers-go (needs Docker).

---

## 2. Five-minute quickstart

### 2.1 Provision a replication user on PostgreSQL

```sql
CREATE ROLE pgbackup REPLICATION LOGIN PASSWORD '<strong>';
```

Add a `pg_hba.conf` line that allows the agent host to replicate as
that role:

```
host  replication  pgbackup  10.0.0.5/32  scram-sha-256
```

Reload PG (`SELECT pg_reload_conf()`).

### 2.2 Create a repository

```sh
pg_hardstorage repo init file:///srv/backups
```

The repo is a directory (or S3 bucket) that holds chunks, manifests,
and WAL. One repo can hold many deployments. `repo init` is idempotent
on the URL — re-running against an existing repo returns
`conflict.repo_exists` (exit 7).

S3 works the same way:

```sh
pg_hardstorage repo init 's3://acme-backups/?region=eu-central-1'
```

Other backends use the same shape — pick the URL scheme
that matches your storage:

| Backend | Example URL |
| --- | --- |
| Local filesystem | `file:///srv/backups` |
| AWS S3 / MinIO / R2 / B2 | `s3://acme-backups/?region=eu-central-1` |
| Google Cloud Storage | `gcs://acme-backups/` |
| Azure Blob | `azblob://account.blob.core.windows.net/container/` |
| Remote host via SSH (SFTP) | `sftp://backup@nas.example.com/srv/backups` |
| Remote host via SSH (ssh-exec) | `scp://backup@nas.example.com/srv/backups` |

`sftp://` and `scp://` both ride SSH; pick `sftp://` by
default and `scp://` when the remote disables the SFTP
subsystem.  See
[Add an SFTP repository](../how-to/adding/repository-sftp.md)
and [Add an SCP repository](../how-to/adding/repository-scp.md)
for the auth / known_hosts / extras-map setup.

### 2.3 Validate PG is ready to stream (one-shot preflight)

`wal stream` runs an automatic preflight on every start, but you
can run it standalone first to confirm the source PostgreSQL
satisfies the replication requirements before you wire systemd:

```sh
pg_hardstorage wal preflight db1 \
    --pg-connection 'postgres://pgbackup@db1.example.com/postgres'
```

Fatal findings (`wal_level.too_low`, `max_replication_slots.full`,
`max_wal_senders.saturated`, `role.no_replication`) make the
command exit non-zero with a `suggestion:` block on each finding.
Warnings (`max_slot_wal_keep_size.set`,
`idle_replication_slot_timeout.set` on PG 17+) surface but don't
block.

### 2.4 Start the WAL streamer (this is the always-on core)

With preflight clean, start the WAL streamer.  It is the headline
feature of `pg_hardstorage` and the process you keep running 24/7:

```sh
pg_hardstorage wal stream db1 \
    --pg-connection 'postgres://pgbackup@db1.example.com/postgres' \
    --repo file:///srv/backups
```

The agent issues `CREATE_REPLICATION_SLOT pg_hardstorage_db1
PHYSICAL RESERVE_WAL` if the slot is absent — `RESERVE_WAL` pins
the slot's `restart_lsn` immediately at create time, so PG
retains WAL from that moment on.  Then it issues
`START_REPLICATION SLOT pg_hardstorage_db1 PHYSICAL` against the
slot.  The stream is gap-free across agent restarts.  Supervise it
with systemd (the package ships `pg_hardstorage@<deployment>.service`
for exactly this) or your container scheduler.

`--skip-preflight` is the explicit override if you've already
audited PG; `--no-slot` is the explicit escape hatch for
archive-only deployments that guarantee WAL retention through
another mechanism (both emit loud warnings — using either is
deliberate).

Leave it running.  The remaining steps run in a second terminal
or under a separate scheduler.

### 2.5 Take the first base backup

With the streamer running concurrently, take a base backup.  The
two processes share the repo URL but do not coordinate beyond
that — `backup` streams a `BASE_BACKUP` over its own replication
connection while `wal stream` keeps shipping WAL.

The wizard probes PG, generates a signing keypair and a KEK, writes
`pg_hardstorage.yaml`, and (by default) takes the first backup:

```sh
pg_hardstorage init \
    --pg-connection 'postgres://pgbackup@db1.example.com/postgres' \
    --repo file:///srv/backups \
    --deployment db1 \
    --yes
```

To take a backup later without going through the wizard (this
is the command your scheduler runs nightly):

```sh
pg_hardstorage backup db1 \
    --pg-connection 'postgres://pgbackup@db1.example.com/postgres' \
    --repo file:///srv/backups
```

In production: schedule `backup` (cron / systemd timer / k8s
CronJob), supervise `wal stream` (systemd / k8s Deployment).
The base backup is the periodic anchor; the streamer is what
makes PITR byte-precise between anchors.

### 2.6 Restore

```sh
pg_hardstorage restore db1 latest \
    --target /var/lib/postgresql/restored \
    --repo file:///srv/backups
```

PITR via natural-language time:

```sh
pg_hardstorage restore db1 latest \
    --target /var/lib/postgresql/restored \
    --repo file:///srv/backups \
    --to "5 minutes ago"
```

Or to a specific LSN: `--to-lsn 0/3000028`. Or to a named restore
point: `--to-name pre_release`.

The restore writes a managed `recovery.signal` and a managed block in
`postgresql.auto.conf` whose `restore_command` invokes
`pg_hardstorage wal fetch <deployment> %f %p --repo ...`. Start PG;
recovery proceeds.

A `pg_verifybackup` check runs against the data dir before the restore
declares success. Skip it with `--verify=skip` only if you know what
you are doing — exit 9 means the verifier said no.

---

## 3. Verifying the install

```sh
$ pg_hardstorage version
pg_hardstorage v1.0.11 (abc1234, built 2026-04-29T12:00:00Z)
```

`doctor` is the single-command "is anything wrong" check. It prints a
sectioned report — resolved PATHS, CONFIG, KEYSTORE, AIRGAP posture,
REPOS reachability, and an ISSUES list:

```sh
$ pg_hardstorage doctor
Mode: user

CONFIG
  Status:        configured
  Schema:        pg_hardstorage.config.v1
  Deployments:   1 (db1)
    db1                  class: internal
  Source files:
    [loaded ] ~/.config/pg_hardstorage/pg_hardstorage.yaml

KEYSTORE
  Signing key:   ✓ present
  KEK:           ✓ present (encryption ON by default for new backups)

REPOS
  file:///srv/backups — reachable
    audit chain: 5 event(s)

ISSUES
  [WARNING] audit.anchor_missing: 5 audit event(s) but no transparency-log anchor; run `pg_hardstorage audit anchor`
    hint: run `pg_hardstorage audit anchor --repo <url>`
```

Each issue line carries an indented `hint:` line underneath. Run
`pg_hardstorage doctor -o json` for a machine-readable form.

`doctor` exits 0 when healthy and exit 10 with `--exit-on-issues` when
there are findings — wire that into your alerting if you want a
hard fail signal.

---

## 4. What's next

- [Operator guide](../operations/operator-guide.md) — daily operations,
  retention, verification, encryption, sinks, troubleshooting pointers.
- [Architecture](../explanation/architecture-tour.md) — how the data
  plane is built, why it talks the replication protocol, what the
  on-disk layout looks like.
- [Runbooks](../reference/runbooks/index.md) — copy-paste procedures
  for the seven scenarios that wake an on-call DBA at 3am.
- [API reference](../reference/api/index.md) — REST surface for the
  control plane.
