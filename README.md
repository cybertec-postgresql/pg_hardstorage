# pg_hardstorage

> PostgreSQL backup, done right.

[![CI](https://github.com/cybertec-postgresql/pg_hardstorage/actions/workflows/ci.yml/badge.svg)](https://github.com/cybertec-postgresql/pg_hardstorage/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/cybertec-postgresql/pg_hardstorage?display_name=tag&sort=semver)](https://github.com/cybertec-postgresql/pg_hardstorage/releases)
[![Go Reference](https://pkg.go.dev/badge/github.com/cybertec-postgresql/pg_hardstorage.svg)](https://pkg.go.dev/github.com/cybertec-postgresql/pg_hardstorage)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)
[![Go Report Card](https://goreportcard.com/badge/github.com/cybertec-postgresql/pg_hardstorage)](https://goreportcard.com/report/github.com/cybertec-postgresql/pg_hardstorage)

`pg_hardstorage` is an enterprise-grade PostgreSQL backup tool — a
single static Go binary, PostgreSQL 15–18, Apache 2.0.

**The core idea: continuous WAL streaming is the always-on data
plane.** A long-running `pg_hardstorage wal stream` process holds a
physical replication slot and ships every byte of WAL into the
repository as PostgreSQL writes it. Periodic base backups are just
the anchor the stream rolls forward from. Daily base backup + a 24/7
stream = byte-precise point-in-time recovery with no gaps.

Everything happens over the **PostgreSQL replication protocol on an
ordinary libpq connection** — the agent never needs OS access to the
database host, and the same binary scales from a laptop to a
100 TB production fleet.

Maintained by **[CYBERTEC PostgreSQL International GmbH](https://www.cybertec-postgresql.com)**.

---

## Quick start

### One-liner install

```sh
curl -sSL https://get.pghardstorage.org | sh
pg_hardstorage version
```

### Homebrew (macOS)

```sh
brew install cybertec-postgresql/tap/pg_hardstorage
```

### From source

```sh
./compile.sh           # build bin/pg_hardstorage (needs Go 1.26+)
```

### 90-second demo

Bring up a temporary PostgreSQL 18 and run the full backup/restore/verify
flow in Docker:

```sh
pg_hardstorage demo
```

The demo prints progress and a result summary. No existing PostgreSQL or
pg_hardstorage configuration is needed — just a running Docker daemon.

### One-command setup

```sh
pg_hardstorage init --quick
```

Auto-detects a local PostgreSQL 18, creates a file:// repo, takes the first
backup, and prints the next steps. Zero prompts. Zero decisions.

### Interactive helper

```sh
make build-simple  # build the interactive helper
./bin/pg_hardstorage_simple
```

No flags, no subcommands — pick a number and answer prompts:

```
  What would you like to do?

     1. Set up backups for a database I haven't backed up before
     2. Take a backup right now
     3. Start continuous protection (base backup + WAL streaming)
     4. See what's in my repository
     5. Verify a backup is restorable
     6. Restore a backup
     q. quit
```

### Docker Compose evaluation stack

```sh
docker compose up
```

Brings up PostgreSQL 18 + pg_hardstorage agent + MinIO (S3-compatible
repo) + Prometheus + Grafana. Full evaluation environment in one
command.

The agent is started with `--metrics-listen 0.0.0.0:9187`, so the
`pg_hardstorage_*` metrics are reachable and scraped out of the box:

| Service | URL | Notes |
| --- | --- | --- |
| Agent `/metrics` | <http://localhost:9187/metrics> | Prometheus text exposition |
| Prometheus | <http://localhost:9090> | scrapes the agent every 15s |
| Grafana | <http://localhost:3000> | `admin`/`admin`; Prometheus datasource + a *pg_hardstorage overview* dashboard are provisioned at boot |
| MinIO console | <http://localhost:9001> | `minioadmin`/`minioadmin` |

No PostgreSQL handy? Bring up a throwaway PG + agent + repo entirely
on your local Docker daemon:

```sh
./scripts/devcluster.sh up
```

Prefer the explicit path? The canonical loop is **init the repo →
start the streamer → take a base backup → restore**:

```sh
# 1. create the repository
pg_hardstorage repo init file:///srv/pg-backups/db1

# 2. start continuous WAL streaming (long-running — supervise with systemd)
pg_hardstorage wal stream db1 \
  --pg-connection "host=10.0.0.10 user=replicator dbname=postgres" \
  --repo file:///srv/pg-backups/db1

# 3. take a base backup (runs concurrently with the streamer)
pg_hardstorage backup db1 \
  --pg-connection "host=10.0.0.10 user=replicator dbname=postgres" \
  --repo file:///srv/pg-backups/db1

# 4. verify it, then prove it restores
pg_hardstorage verify  db1 latest --repo file:///srv/pg-backups/db1
pg_hardstorage restore db1 latest --repo file:///srv/pg-backups/db1 \
  --target /var/lib/postgresql/restored
```

`pg_hardstorage init` runs the same connect → init-repo → first-backup
flow as a guided wizard. The
[getting-started tutorial](docs/tutorials/getting-started.md) walks
the whole round-trip end to end.

---

## Highlights

- **Native WAL streaming** over the PostgreSQL replication protocol
  — the headline feature; everything else exists to serve it.
- **Base backups** that interleave with the live stream — the
  streamer keeps running while `backup` executes.
- **Point-in-time recovery** to a time, an LSN, or a named restore
  point — with a `--preview` dry-run.
- **Patroni-aware failover** — four cooperating mechanisms keep the
  stream gap-free across leader switches.
- **Drop-in compatibility** with pgBackRest, Barman and WAL-G — the
  same CLI surface, plus a config translator.
- **Kubernetes** — runs as a CronJob / Deployment; verified
  end-to-end against CloudNativePG.
- Content-addressed **deduplication** (FastCDC, page-aligned splits)
  — no incremental chains to break.
- **AES-256-GCM** envelope encryption; a FIPS / BoringCrypto build
  variant is available.
- **4 Tier-1 KMS providers** — AWS KMS · GCP KMS · Azure Key Vault ·
  HashiCorp Vault Transit. (A PKCS#11 / HSM provider is in progress.)
- **6 Tier-1 storage backends** — filesystem · S3 · GCS · Azure Blob
  · SFTP · SCP.
- **LLM-assisted** operator surface for the 3am restore — read-only
  by default, every command previewed before it runs.
- Structured output everywhere (`--output json|ndjson|yaml|…`,
  11 renderers) and 14 notification sinks.
- Schema-versioned wire formats with a **24-month
  backward-compatibility** commitment.

> Works against any PostgreSQL that exposes the replication protocol
> — bare metal, VMs, Patroni clusters, and PostgreSQL behind a
> Kubernetes operator. Fully-managed DBaaS offerings (RDS, Cloud SQL,
> …) that do not expose the replication protocol are not supported.

---

## WAL streaming — the two-process model

In production you run **two** `pg_hardstorage` processes side by
side, against the same repository:

```text
┌────────── PostgreSQL ──────────┐
│   data dir + pg_wal/           │
└──┬──────────────────────────┬──┘
   │ replication protocol     │ replication protocol
   ▼ (BASE_BACKUP)            ▼ (START_REPLICATION SLOT … PHYSICAL)
┌─────────────────────────┐  ┌────────────────────────────┐
│  pg_hardstorage backup  │  │  pg_hardstorage wal stream │
│  scheduled (e.g. daily) │  │  always-on, never stops    │
└────────────┬────────────┘  └──────────────┬─────────────┘
             │                              │
             └─────► same repo URL ◄────────┘
```

The streamer holds a physical slot created with `RESERVE_WAL`, so
PostgreSQL retains every WAL segment from `restart_lsn` onwards from
the moment the slot exists — *not* from the moment the first stream
byte flows. A crash of the streamer is just a restart; no gaps. The
base backup runs concurrently; the two processes never coordinate
beyond both pointing at the same repository.

Before opening the stream, the agent runs a configuration
**preflight** — `wal_level`, `max_replication_slots`,
`max_wal_senders`, the role's `REPLICATION` attribute, plus
`max_slot_wal_keep_size` / `idle_replication_slot_timeout` warnings —
and a start-LSN safety check against the slot's `restart_lsn`. The
same preflight is reachable standalone via `pg_hardstorage wal
preflight <deployment>` for setup runbooks and CI gates.

---

## Patroni

`pg_hardstorage` is built for highly-available clusters. When a
Patroni leader switch happens, four cooperating mechanisms keep the
WAL stream gap-free:

- **permanent slots** — the replication slot exists on every node,
  including the one that becomes the new leader;
- **PG 17+ synced slots** — PostgreSQL itself keeps the slot in sync
  across the cluster;
- **recreate-on-detection** — the streamer reconnects through
  Patroni's REST API (never a stale hostname) and recreates the slot
  if it has to;
- **gap auditor** — a periodic check emits a `wal.gap_detected`
  event if any of the above is misconfigured.

`pg_hardstorage patroni` gives a read-only view of a Patroni-managed
cluster. See the [Patroni tutorial](docs/tutorials/patroni-cluster.md)
and the [failover deep-dive](docs/explanation/patroni-failover-deep-dive.md).

---

## Drop-in compatibility: pgBackRest · Barman · WAL-G

Migrating off another tool does not mean rewriting your automation.
`make build-compat` produces multicall shim binaries that present the
**same command-line surface** as the tool they replace:

| Shim | Replaces |
| --- | --- |
| `pg-hardstorage-pgbackrest` | `pgbackrest` |
| `pg-hardstorage-barman` / `pg-hardstorage-barman-wal-archive` | `barman` / `barman-wal-archive` |
| `pg-hardstorage-walg` | `wal-g` |

Drop the shim in where the old tool was — in an `archive_command`, a
cron job, or an operator's container image — and `pg_hardstorage`
runs underneath. `pg_hardstorage compat translate --from <tool>
<config-path>` reads an existing `pgbackrest.conf` / `barman.conf` and
emits a ready-to-review `pg_hardstorage.yml`. See the
[migration how-to guides](docs/how-to/migration/index.md).

---

## Kubernetes

PostgreSQL behind a Kubernetes operator is ordinary PostgreSQL in a
pod — `pg_hardstorage` backs it up over the replication protocol like
any other instance:

- Run it in its **own pod** — a **CronJob** for scheduled base
  backups, a **Deployment** for continuous WAL streaming — pointed at
  the database Service. No sidecar, no operator plugin required.
- **Verified end-to-end against CloudNativePG**: backup → verify →
  restore round-trips against a CNPG cluster. The reproducible script
  lives at [`test/k8s/`](test/k8s/README.md).
- An **in-tree Helm chart** ([`charts/pg-hardstorage-sidecar`](charts/pg-hardstorage-sidecar/))
  deploys the agent as a StatefulSet.
- The pgBackRest / Barman / WAL-G [compat shims](#drop-in-compatibility-pgbackrest--barman--wal-g)
  slot into operator images that expect those tools.

A native CloudNativePG-I provider is on the roadmap; today the
CronJob / Deployment model above is the verified path. See the
[Kubernetes how-to guides](docs/how-to/kubernetes/index.md) and the
[CNPG tutorial](docs/tutorials/kubernetes-cnpg.md).

---

## Install & build

`pg_hardstorage` is a single static binary. Requires **Go 1.26+** to
build.

```sh
./compile.sh                # downloads deps, builds bin/pg_hardstorage
./compile.sh --testkit      # also build bin/pg_hardstorage_testkit
./compile.sh --fips         # FIPS / BoringCrypto variant (Linux/amd64)
./compile.sh --pkcs11       # PKCS#11 / HSM variant (cgo + libpkcs11; in progress)
./compile.sh --firecracker  # microVM verifier-sandbox variant
./compile.sh --help         # full options
```

Or via the canonical Makefile:

```sh
make                   # build bin/pg_hardstorage + bin/pg_hardstorage_testkit
make build-simple      # the interactive quick-start helper
make build-compat      # the pgBackRest / Barman / WAL-G shims
make build-fips        # FIPS / BoringCrypto variant
make build-firecracker # microVM verifier-sandbox variant
make test              # go test -race -count=1 ./...
make test-integration  # adds -tags=integration; requires Docker
```

Other ways to run it:

- **Containers** — build from
  [`deploy/docker/Dockerfile`](deploy/docker/Dockerfile) (distroless,
  runs as `nonroot`); see the [Kubernetes guides](docs/how-to/kubernetes/index.md).
- **systemd** — [`deploy/systemd/`](deploy/systemd/) ships
  `pg_hardstorage.service` plus a `pg_hardstorage@<deployment>.service`
  template for multi-instance hosts.
- **Linux packages** — the release pipeline produces signed `.deb`
  and `.rpm` artefacts; see the
  [packaging guide](docs/how-to/packaging/debian-rpm.md).

Release artefacts are cosign-signed (keyless / Sigstore) and ship an
SPDX SBOM.

---

## Daily operations cheat-sheet

The most-cited commands. See the
[operator guide](docs/operations/operator-guide.md) for the full
reference.

```sh
# Validate PG is ready to stream (also runs automatically inside `wal stream`)
pg_hardstorage wal preflight db1 --pg-connection ...

# Stream WAL continuously (long-running — supervise with systemd)
pg_hardstorage wal stream db1 --pg-connection ... --repo ...

# Take a base backup right now
pg_hardstorage backup db1 --pg-connection ... --repo ...

# Restore latest, or PITR by time / LSN; --preview for a dry-run
pg_hardstorage restore db1 latest --repo ... --target /var/lib/postgresql/restored
pg_hardstorage restore db1 latest --repo ... --target /tmp/r --to "5 minutes ago"
pg_hardstorage restore db1 latest --repo ... --target /tmp/r --to-lsn 0/3000028
pg_hardstorage restore db1 latest --repo ... --target /tmp/r --preview

# Inspect + verify
pg_hardstorage status                       # all deployments
pg_hardstorage list   db1 --repo ...
pg_hardstorage verify db1 latest --repo ...
pg_hardstorage doctor                       # self-diagnosis

# Retention (dry-run by default; --apply to delete)
pg_hardstorage rotate db1 --repo ... --policy gfs --apply
```

Every command supports `--output json` / `--output ndjson` — the
schema is `pg_hardstorage.v1` with a 24-month back-compat commitment.

---

## Documentation

The doc site is [Diátaxis](https://diataxis.fr/)-organised — every
page is one of *tutorial / how-to / reference / explanation*.

| Quadrant | What lives there |
| --- | --- |
| **[Tutorials](docs/tutorials/index.md)** | Learn-by-doing: getting started, first backup + restore, PITR, encryption, Patroni, Kubernetes, plugin authoring, LLM incident response |
| **[How-to guides](docs/how-to/index.md)** | Task-oriented recipes: adding repos / KMS / sinks; operating; Kubernetes; air-gapped; packaging; migration |
| **[Reference](docs/reference/index.md)** | CLI (200+ auto-generated pages), REST API, plugin contracts, schema catalogues (manifest / output event / KEKRef / storage URL / exit codes / error codes / metrics) |
| **[Explanation](docs/explanation/index.md)** | Conceptual deep-dives: design principles, the WAL pipeline, Patroni failover, envelope encryption, the audit chain, the LLM safety stack, threat model, comparison vs pgBackRest / WAL-G / Barman |

Plus the [operations handbook](docs/operations/index.md), the
[compliance docs](docs/compliance/index.md), the
[3am-operator runbooks](docs/reference/runbooks/index.md), the
[FAQ](docs/faq.md) and the [glossary](docs/glossary.md).

---

## Contributing

Read [`CONTRIBUTING.md`](CONTRIBUTING.md) and
[`docs/CONTRIBUTING-DOCS.md`](docs/CONTRIBUTING-DOCS.md) for the
authoring conventions. Bug reports land best with a runnable testkit
scenario — the testkit binary is built via `make build-testkit`.

Security disclosures: [`SECURITY.md`](SECURITY.md).

---

## License

Apache 2.0. See [`LICENSE`](LICENSE).
Copyright © 2026 [CYBERTEC PostgreSQL International GmbH](https://www.cybertec-postgresql.com).
