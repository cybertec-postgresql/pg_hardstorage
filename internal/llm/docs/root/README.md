# pg_hardstorage

> PostgreSQL backup, done right.

[![CI](https://github.com/cybertec-postgresql/pg_hardstorage/actions/workflows/ci.yml/badge.svg)](https://github.com/cybertec-postgresql/pg_hardstorage/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/cybertec-postgresql/pg_hardstorage?display_name=tag&sort=semver)](https://github.com/cybertec-postgresql/pg_hardstorage/releases)
[![Go Reference](https://pkg.go.dev/badge/github.com/cybertec-postgresql/pg_hardstorage.svg)](https://pkg.go.dev/github.com/cybertec-postgresql/pg_hardstorage)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)
[![Go Report Card](https://goreportcard.com/badge/github.com/cybertec-postgresql/pg_hardstorage)](https://goreportcard.com/report/github.com/cybertec-postgresql/pg_hardstorage)

`pg_hardstorage` is an enterprise-grade PostgreSQL backup tool —
single static Go binary, PostgreSQL 15+, Apache 2.0.

**The core idea:** continuous **WAL streaming** is the always-on
data plane.  A long-running `pg_hardstorage wal stream` process
holds a physical replication slot and ships every byte of WAL into
the repo as PostgreSQL writes it.  Periodic base backups are just
the anchor that the stream rolls forward from.  Daily backup +
24/7 stream = byte-precise PITR with no gaps.

**What it does:**

- Native **WAL streaming** over the PostgreSQL replication protocol
  — the headline feature; everything else exists to serve it
- Periodic **base backups** that interleave with the live stream
  (the streamer keeps running while `backup run` executes)
- Content-addressed **deduplication** (FastCDC + page-aligned splits)
- **AES-256-GCM** envelope encryption, FIPS variant available
- 5 Tier-1 **KMS providers** — AWS · GCP · Azure · Vault Transit · PKCS#11/HSM
- 6 Tier-1 **storage backends** — fs · S3 · GCS · Azure Blob · SFTP · SCP (ssh-exec)
- **Patroni-aware** failover (4 cooperating mechanisms)
- **LLM-assisted** operator surface for the 3am restore (read-only by default)

**What makes it different:**

- Data plane is the **PostgreSQL replication protocol over a libpq
  connection** — the agent never needs OS access to the database host
- Runs against **PostgreSQL you operate yourself** — bare-metal, VMs,
  Patroni clusters, and Kubernetes (CNPG). Fully-managed DBaaS (Amazon
  RDS/Aurora, GCP Cloud SQL, Azure Database, Aiven, Supabase, Neon) are
  **not supported**: they do not expose `BASE_BACKUP` / physical
  replication to customers, so a physical base backup is impossible
- One binary scales **laptop → 100 TB production fleet**
- Schema-versioned wire formats with a **24-month backward-compatibility**
  commitment

Maintained by **CYBERTEC PostgreSQL International GmbH**.  Maintainer:
Hans-Jürgen Schönig &lt;hs@cybertec.at&gt; (GitHub
[postgresql007](https://github.com/postgresql007)).
See the [v1.0 release notes](docs/release-notes/v1.0.md) for the
headline feature inventory.

---

## Pick your platform

The fastest path depends on where you run PostgreSQL.  Find your
bucket, click through.

### Kubernetes

- **CloudNativePG (CNPG)**
    - [CNPG-I provider](docs/how-to/kubernetes/cnpg-i-provider.md) — native plugin; CNPG `Cluster` CR unchanged
    - [End-to-end tutorial](docs/tutorials/kubernetes-cnpg.md) — `kind` → first backup → first restore
- **Zalando postgres-operator**
    - [WAL-G shim](docs/how-to/kubernetes/walg-shim.md) — drop `pg-hardstorage-walg` in for `wal-g`; cluster spec unchanged
- **Crunchy PGO**
    - [pgBackRest shim](docs/how-to/kubernetes/pgbackrest-shim.md) — drop `pg-hardstorage-pgbackrest` in for `pgbackrest`; `PostgresCluster` CR unchanged
- **In-pod Barman wrapper**
    - [Barman shim](docs/how-to/kubernetes/barman-shim.md) — drop in for `barman` / `barman-wal-archive` inside any custom image
- **Helm (any distro)**
    - [Sidecar chart](docs/how-to/kubernetes/helm-sidecar-chart.md) — StatefulSet alongside an external PG endpoint
    - [Server chart](docs/how-to/kubernetes/helm-server-chart.md) — control-plane chart

### Linux distributions

- **RHEL / Rocky / AlmaLinux / Fedora / Amazon Linux**
    - [RPM packaging guide](docs/how-to/packaging/debian-rpm.md) — `dnf install` from a release artefact
    - [`packaging/rpm/pg_hardstorage.spec`](packaging/rpm/pg_hardstorage.spec) — the spec we ship; multi-binary split with compat sub-packages
- **Debian / Ubuntu**
    - [`.deb` packaging guide](docs/how-to/packaging/debian-rpm.md) — `apt install` from a release artefact
    - [`debian/`](debian/) — lintian-clean source-package tree, including `pg-hardstorage-compat-{pgbackrest,barman,walg}`
- **Generic bare-metal Linux**
    - [systemd units](deploy/systemd/) — `pg_hardstorage.service` plus the `pg_hardstorage@<deployment>.service` template for multi-instance hosts
    - [`./compile.sh`](compile.sh) — clone-and-build; verifies Go ≥ 1.26 and produces `bin/pg_hardstorage`

### Containers

- **Docker / Podman / containerd**
    - [`deploy/docker/Dockerfile`](deploy/docker/Dockerfile) — distroless static base, runs as `nonroot`
    - Pre-built image: `docker pull ghcr.io/cybertec-postgresql/pg_hardstorage:<version>`

### Build from source

- **One-liner**
    - [`./compile.sh`](compile.sh) — verifies Go ≥ 1.26, fetches deps, builds `bin/pg_hardstorage`; pass `--help` for variant flags
- **Make targets**
    - `make build` — main `pg_hardstorage` binary
    - `make build-fips` — FIPS / BoringCrypto variant (Linux/amd64)
    - `make build-pkcs11` — HSM-backed KMS via PKCS#11 (cgo + libpkcs11)
    - `make build-firecracker` — microVM verifier-sandbox variant
    - `make build-compat` — drop-in shims for pgBackRest, Barman, and WAL-G

### Compliance & special environments

- **FIPS 140-2**
    - [BoringCrypto variant](docs/how-to/packaging/fips-variant.md) — `pg-hardstorage-fips` build, validated module path
- **HSM / PKCS#11**
    - [PKCS#11 variant](docs/how-to/packaging/pkcs11-variant.md) — works with Thales, nCipher, AWS CloudHSM, and similar
- **Air-gapped**
    - [Enable air-gap policy](docs/how-to/air-gapped/enable-policy.md) — refuses every non-local network call at start-up
    - [Bundle export](docs/how-to/air-gapped/repo-bundle-export.md) — shippable archive of a backup + manifest + KMS refs
- **Patroni clusters**
    - [Tutorial](docs/tutorials/patroni-cluster.md) — three-node cluster from scratch
    - [Failover deep-dive](docs/explanation/patroni-failover-deep-dive.md) — slot continuity, timelines, gap detection
- **Self-managed PostgreSQL (bare-metal / VM / single node)**
    - [Getting started](docs/tutorials/getting-started.md) — the replication-protocol data plane needs no host access (but the server must expose `BASE_BACKUP`, which fully-managed DBaaS such as RDS/Aurora/Cloud SQL do not)

---

## Try it in 60 seconds

If your platform isn't above, or you just want a feel:

```sh
# Throwaway PG + agent + repo via Docker (clean shutdown via `down`):
./scripts/devcluster.sh up

# Or build from source (Go ≥ 1.26):
./compile.sh
./bin/pg_hardstorage --help
```

Then walk the [getting-started tutorial](docs/tutorials/getting-started.md)
for the canonical repo-init → **start the streamer** → first
base backup → restore round-trip with real PostgreSQL.

## The two-process model

In production you run **two** `pg_hardstorage` processes side by
side, against the same repo:

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

The streamer holds a physical slot created with `RESERVE_WAL`,
so PostgreSQL retains every WAL segment from `restart_lsn`
onwards from the moment the slot exists — *not* from the moment
the first stream byte flows.  A crash of the streamer is just a
restart — no gaps.  The base backup runs concurrently with the
streamer; the two processes never coordinate beyond both
pointing at the same repo.

Before opening the stream, the agent runs a configuration
**preflight** (`wal_level`, `max_replication_slots`,
`max_wal_senders`, role `REPLICATION` attribute,
`max_slot_wal_keep_size` / `idle_replication_slot_timeout`
warnings) and a start-LSN safety check against the slot's
`restart_lsn`.  The same preflight is reachable standalone via
`pg_hardstorage wal preflight <deployment>` for setup runbooks
and CI gates.

---

## What's in v1.0

- **5 Tier-1 KMS providers** — AWS KMS, GCP KMS, Azure Key Vault, Vault Transit, PKCS#11/HSM
- **6 Tier-1 storage backends** — fs, S3, GCS, Azure Blob, SFTP, SCP
- **Native WAL streaming** + four cooperating Patroni-failover mechanisms
- **LLM-assisted operations** — read-only by default, advise+execute opt-in, 5-gate safety stack, signed evidence bundles
- **Two verifier sandboxes** — Docker (default) + Firecracker microVM (`-tags firecracker`)

Plus: SCIM 2.0 · i18n (en/de/fr/ja) · cross-account replication ACL ·
threshold-attestation gate · n-of-m approvals · JIT access · legal hold
· data-classification tags · GDPR Art. 17 crypto-shred · audit
evidence bundles · SLSA L3 build provenance · 14 sinks · 11 renderers
· Tier-2 plugin protocol over stdio JSON-RPC.

Full inventory + upgrade notes: [v1.0 release notes](docs/release-notes/v1.0.md).

---

## Documentation

The doc site is [Diátaxis](https://diataxis.fr/)-organised — every
page is one of *tutorial / how-to / reference / explanation*.

| Quadrant | What lives there |
| --- | --- |
| **[Tutorials](docs/tutorials/)** | Learn-by-doing: getting started, first backup + restore, PITR, encryption, Patroni, K8s, plugin authoring, LLM incident response |
| **[How-to guides](docs/how-to/)** | ~50 task-oriented recipes: adding repos / KMS / sinks; operating; Kubernetes; air-gapped; packaging; migration |
| **[Reference](docs/reference/)** | CLI (202 auto-generated pages) · REST API · 9 plugin contracts · schema catalogues (manifest / output event / skill / KEKRef / storage URL / build flavour / exit codes / error codes / metrics) |
| **[Explanation](docs/explanation/)** | Conceptual deep-dives: design principles, WAL pipeline, Patroni failover, envelope encryption, audit chain, LLM safety stack, threat model, comparison vs pgBackRest / WAL-G / Barman |

Plus:

- **[Operations handbook](docs/operations/)** — operator guide, troubleshooting, monitoring, alerting recipes, capacity, cost, SLO-as-code, incident response
- **[Compliance](docs/compliance/)** — SOC 2, ISO 27001, HIPAA, PCI DSS, FedRAMP, GDPR Art. 17, data residency
- **[Runbooks (R1-R7)](docs/reference/runbooks/)** — 3am-operator playbooks for the named disaster scenarios
- **[FAQ](docs/faq.md)** · **[Glossary](docs/glossary.md)** · **[Documentation plan](docs/DOC_PLAN.md)**

---

## Daily operations cheat-sheet

The most-cited commands.  See the
[operator guide](docs/operations/operator-guide.md) for the
full reference.

```sh
# Validate PG is ready to stream (one-shot; runs automatically inside `wal stream` too)
pg_hardstorage wal preflight db1 --pg-connection ...

# Stream WAL continuously (long-running, supervise with systemd)
pg_hardstorage wal stream db1 --pg-connection ... --repo ...

# Backup right now
pg_hardstorage backup db1 --pg-connection ... --repo ...

# Restore latest, or PITR via natural-language time
pg_hardstorage restore db1 latest --repo ... --target /var/lib/postgresql/restored
pg_hardstorage restore db1 latest --repo ... --target /tmp/r --to "5 minutes ago"
pg_hardstorage restore db1 latest --repo ... --target /tmp/r --to-lsn 0/3000028
pg_hardstorage restore db1 latest --repo ... --target /tmp/r --preview   # dry-run

# Inspect + verify
pg_hardstorage status                                  # all deployments
pg_hardstorage list db1 --repo ...
pg_hardstorage verify db1 latest --repo ...
pg_hardstorage doctor                                  # self-diagnosis

# Retention (dry-run by default; --apply to delete)
pg_hardstorage rotate db1 --repo ... --policy gfs --apply
```

Every command supports `--output json` / `--output ndjson` — the
schema is `pg_hardstorage.v1` with a 24-month back-compat commitment.

---

## Building

Requires Go 1.26+.

```sh
./compile.sh               # downloads deps, builds bin/pg_hardstorage
./compile.sh --testkit     # also build bin/pg_hardstorage_testkit
./compile.sh --fips        # FIPS variant (Linux/amd64 only)
./compile.sh --pkcs11      # HSM variant (cgo + libpkcs11)
./compile.sh --firecracker # microVM verifier-sandbox variant
./compile.sh --help        # full options
```

Or via the canonical Makefile:

```sh
make                       # build bin/pg_hardstorage + bin/pg_hardstorage_testkit
make test                  # go test -race -count=1 ./...
make test-integration      # adds -tags=integration; requires Docker
make docs-build            # render the MkDocs site to ./site/
make docs-doctest          # execute every `# RUNNABLE` block in tutorials
```

---

## Contributing

Read [`CONTRIBUTING.md`](CONTRIBUTING.md) and
[`docs/CONTRIBUTING-DOCS.md`](docs/CONTRIBUTING-DOCS.md) for the
authoring conventions.  Bug reports best land with a runnable
testkit scenario — the testkit binary is shipped via
`make build-testkit`.

Security disclosures: [`SECURITY.md`](SECURITY.md).

---

## License

Apache 2.0.  See [`LICENSE`](LICENSE).
Copyright © 2026 CYBERTEC PostgreSQL International GmbH.
