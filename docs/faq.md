---
title: Frequently asked questions
description: Answers to the questions a real evaluator, operator, or auditor asks in their first ten minutes with pg_hardstorage.
tags:
  - faq
  - getting-started
  - evaluation
---

# Frequently asked questions

Audience-organised so a reader can scan to the section that
matches them.  Every answer cross-links to the page that
goes deeper.  When two pages exist on the same topic — one
recipe, one rationale — the FAQ links the recipe; explore
the [explanation/](explanation/index.md) tree for the why.

If you arrived here from a search engine and the question
you wanted isn't here, the [glossary](glossary.md) probably
defines the word that brought you, and the
[support page](support/index.md) tells you where to file
a documentation gap.

---

## Getting started

#### Will pg_hardstorage work against AWS RDS, Aurora, GCP Cloud SQL, Azure Database, Neon, or Supabase?

No.  pg_hardstorage's data plane runs over the PostgreSQL
**physical replication protocol** — a physical replication
slot plus `BASE_BACKUP`.  Fully-managed DBaaS providers do
not expose `BASE_BACKUP` (or physical replication) to
customers, so pg_hardstorage cannot take a physical base
backup of them.  It targets PostgreSQL you run yourself.

What the replication-protocol design *does* remove is the
need for host-level access.  On self-managed PostgreSQL —
bare metal, VMs, containers, Patroni clusters, and operators
like CloudNativePG — the agent backs up over a normal database
connection with no SSH, no OS access, and no
`archive_library`/`archive_command` on the host, and it can
run in a different VM, region, or cluster from the database.
This is the single biggest architectural difference from
pgBackRest; see the
[architecture tour](explanation/architecture-tour.md#2-wal-via-the-replication-protocol)
for the full reasoning.

#### What PostgreSQL versions are supported?

PG 15, 16, and 17 are first-class.  PG 18 is supported in
the test matrix from v0.9 onward.  PG 14 and earlier are
out of scope — `BASE_BACKUP` non-exclusive mode and
`pg_backup_start` / `pg_backup_stop` are PG 15-only APIs we
rely on.

#### How do I take my first backup?

Five-minute path: install the binary, run
`pg_hardstorage init` against your database, then
`pg_hardstorage backup <deployment>`.  The full walk-through
with copy-pasteable commands is the
[Getting started tutorial](tutorials/getting-started.md);
the
[first backup + restore](tutorials/first-backup-restore.md)
tutorial extends it to a full restore round-trip.

#### What's the smallest practical install footprint?

Single static binary, less than 100 MB RSS at idle, no
external coordination service, JSON state files under
`<state>/bookkeeping/`.  Embedded mode is the default when
no control plane is configured; restarting the binary is
the entire HA story.  See
[three execution modes](explanation/three-execution-modes.md).

#### Do I need a control plane?

Only when you have multiple agents to coordinate.  A
single-host install runs the agent and a minimal
in-process control plane in the same binary.  The fleet
mode (separate control plane process) becomes useful at two
or more hosts; see
[coordination without etcd](explanation/coordination-without-etcd.md)
for the topology matrix.

#### Where does pg_hardstorage put its files on disk?

XDG-respecting under FHS conventions: configuration in
`/etc/pg_hardstorage/` (system) or
`$XDG_CONFIG_HOME/pg_hardstorage/` (user); state in
`/var/lib/pg_hardstorage/`; logs in
`/var/log/pg_hardstorage/`; runtime sockets in
`/run/pg_hardstorage/`.  Override individually via
`PG_HARDSTORAGE_*` env vars.  The
[operator guide](operations/operator-guide.md) lists every
path.

#### Which storage backends are supported?

Six Tier-1 backends ship in the binary, all selected by
URL scheme:

| Scheme | Backend | Typical use |
| --- | --- | --- |
| `file://` | local filesystem | dev, single-host, NFS-mounted NAS |
| `s3://` | AWS S3, MinIO, Cloudflare R2, Backblaze B2 | cloud, on-prem object storage |
| `gcs://` | Google Cloud Storage | GCP-native deployments |
| `azblob://` | Azure Blob Storage | Azure-native deployments |
| `sftp://` | SSH/SFTP | remote host, SFTP subsystem enabled |
| `scp://` | SSH (shell-exec) | remote host where SFTP is disabled |

`sftp://` is the default-recommended SSH-based backend;
`scp://` exists for hardened SSH deployments where the SFTP
subsystem is off (or embedded servers that don't ship one).
Tier-2 vendor-specific backends plug in via
[`$HSPLUGIN_PATH`](explanation/tier1-vs-tier2-plugins.md).
Full URL grammar + capabilities matrix:
[storage URL schemes reference](reference/storage-url-schemes.md).

---

## Backup & restore

#### How fast is restore for a 1 TB database?

Restore is bandwidth-bound.  On a 10 Gbps NIC with a warm
S3 cache the parallel chunk fetcher saturates the link;
end-to-end is roughly 10–20 minutes including
`pg_verifybackup` once the data dir is hydrated.  The
agent runs a 30-second pre-flight throughput probe and
prints a projected RTO before kicking off; see the
restore preview output in the
[restore tutorial](tutorials/first-backup-restore.md).

#### Can I restore a single table?

Yes — `pg_hardstorage partial restore <deployment> --tables public.orders`
extracts the requested tables from a backup and loads them
into the running database via `pg_dump`-equivalent
extraction.  See the
[`partial restore` CLI reference](reference/cli/pg_hardstorage_partial_restore.md).

#### What's the difference between fast verify and full verify?

Fast verify runs `pg_verifybackup` against the staged
manifest.  No actual restore happens; the check confirms
file checksums and manifest integrity.  Defaults to running
after every backup.

Full verify allocates a sandbox (Docker
`postgres:<major>` by default; opt-in Firecracker microVM
for stronger isolation), restores into it, runs
`pg_verifybackup` plus `pg_amcheck --all --heapallindexed
--rootdescend` plus user smoke SQL, and tears down.
Defaults to weekly.

The full how-to lives at
[verify: fast vs full](how-to/operating/verify-fast-vs-full.md);
the sandbox tradeoffs are in
[verify-sandbox tradeoffs](explanation/verify-sandbox-tradeoffs.md).

#### Can I do point-in-time recovery (PITR)?

Yes — `pg_hardstorage restore <deployment> --to "yesterday 9pm"`
or any other natural-language time, or `--to-lsn 0/3000028`,
or by passing an explicit `<backup-id>` positionally.  The orchestrator picks the latest
full whose `stop_lsn` precedes the target, replays WAL up
to the target, and applies any timeline switches.  Walk-
through: the
[PITR tutorial](tutorials/pitr-tutorial.md).

#### My backup just failed. How do I tell whether the manifest committed?

A failed backup never commits a manifest — the system
either renames `.tmp` to `.json` atomically, or doesn't.
There is no half-state.  `pg_hardstorage list <deployment>`
shows only committed backups.  If you saw a partial upload
in the logs, the chunks are content-addressed and the next
backup deduplicates them; see
[architecture tour](explanation/architecture-tour.md#3-cas-chunk-store).

#### Are incremental backups chained?

No.  Every backup's manifest references all the chunks it
needs; a chunk written by a prior incremental is referenced
directly by hash, never via a "parent" pointer.  Deleting
or corrupting one backup cannot invalidate another.  This
is the main thing pg_hardstorage does differently from
pgBackRest's incremental chain; see
[content-addressed storage](explanation/content-addressed-storage.md).

#### How do I delete an old backup?

`pg_hardstorage backup delete <deployment> <backup-id>`.
The manifest soft-deletes immediately; chunks stay until
the next `repo gc` decides nothing else references them.
See [`backup delete` reference](reference/cli/pg_hardstorage_backup_delete.md)
for `--undelete` (within the trash TTL) and `--force` (skip
confirmation prompt).  Retention can run this for you
automatically — see
[set retention](how-to/operating/set-retention.md).

---

## WAL pipeline

#### Do I need archive_command?

No.  `pg_hardstorage` streams WAL via the replication
protocol over the same database connection used for the
base backup.  A persistent physical replication slot named
`pg_hardstorage_<deployment>` holds WAL on the primary
until our agent ACKs it.

You *can* configure `archive_library` (via the
`pg_hardstorage_archive` C extension) or a classical
`archive_command` shim if your regulatory environment
mandates double-archiving — both feed the same content-
addressed chunk store, dedup makes duplicates free.  See
[wal pipeline](explanation/wal-pipeline.md) for the full
matrix.

#### What happens during a Patroni failover?

The agent watches Patroni's REST API (or the DCS directly)
for leader changes.  On promotion, it reconnects to the new
leader, captures the new timeline's `.history` file into
the repo, and resumes streaming.  Slot continuity has three
strategies — Patroni `permanent_slots` (recommended), PG 17
synced slots, or recreate-on-detection — picked by the
agent based on the cluster's PG and Patroni versions.  See
[Patroni failover deep-dive](explanation/patroni-failover-deep-dive.md).

#### How do I detect WAL gaps?

The gap auditor walks the repo's WAL inventory periodically
and asserts no LSN holes.  Gaps emit `wal.gap_detected`
events and `pg_hardstorage_resilience_slot_recreated_total`
metrics.  The CLI surface is
`pg_hardstorage wal gaps <deployment>`; the doctor check is
`Slot continuity` and prints the gap size in bytes if
non-zero.

#### What's `wal repair` for?

It explicitly recreates a dropped or unhealthy replication
slot and resyncs from the latest backup's `stop_lsn`.  This
is the operator action that *accepts* a WAL gap (whatever
the slot loss caused), as opposed to silently glossing over
it.  Runbook: [R6 — Slot dropped, gap detected](reference/runbooks/R6-slot-dropped-gap.md).

#### Can I get RPO = 0?

Yes, with `wal_mode: synchronous` — the agent advertises
itself as a candidate in `synchronous_standby_names` and
PG will not commit a transaction until our flush ACK
returns.  Trade-off: write latency includes one round-trip
to your repo.  Recommended only for tier-0 deployments
where the customer has explicitly accepted the latency
cost; see
[wal pipeline § sync-target mode](explanation/wal-pipeline.md).

#### What's dual-stream WAL?

Two replication slots on two different nodes (typically
primary + replica) feeding the same content-addressed
chunk store concurrently.  CAS dedup means the duplicate
chunks cost nothing; either stream can fail without an RPO
bump.  Auto-selected at ≥ 50 TB or when
`availability=high` is configured.

#### Why is my pg_wal/ growing without bound?

The replication slot is preserving WAL until the agent
ACKs.  If the agent has been down for a long time, PG has
been holding everything since.  Either restart the agent
to drain the slot, or run `pg_hardstorage wal repair
<deployment>` to drop the slot (and accept the gap).

To prevent the situation in the first place, decide which
trade-off you want:

- Leave `max_slot_wal_keep_size = -1` (the PG default) and
  pair the slot with a disk-free alert plus a streamer-lag
  alert — best when any WAL loss after a clean primary is
  unacceptable.
- Set `max_slot_wal_keep_size` to bound the slot and
  alert before lag approaches the cap — best when bounded
  disk usage is more important than the last few minutes
  of WAL.

`pg_hardstorage wal preflight` and `doctor` surface both
findings (`max_slot_wal_keep_size.set` and `.unbounded`) so
the chosen policy is visible.  See [Replication-slot disk
safety](how-to/operating/slot-disk-safety.md) for the full
guidance and sizing rules of thumb.

---

## Encryption & KMS

#### Where is the KEK stored?

Wherever you point the encryption plugin: AWS KMS, GCP
KMS, Azure Key Vault, HashiCorp Vault Transit, or — for
single-host dev — an `aes-256-gcm` keyring directory.
HSM (PKCS#11) lands in the v1.0 build flavour.  Reference:
[KEKRef schemes](reference/cli/pg_hardstorage_kms.md);
explanation:
[envelope encryption](explanation/envelope-encryption.md).

#### Can I rotate the KEK without rewriting backups?

Yes — `pg_hardstorage kms rotate` walks every manifest,
unwraps each backup's wrapped DEK with the old KEK, re-
wraps with the new KEK, and atomically rewrites the
manifest.  Chunks (which are encrypted under per-chunk
keys derived from the BDEK) are not re-encrypted.  Old
KEK retired after a configurable grace.  How-to:
[rotate KEK](how-to/operating/rotate-kek.md).

#### What does `kms shred` actually delete?

The KEK only.  Every backup whose DEK was wrapped under
that KEK becomes mathematically unrecoverable — chunks and
manifests stay bit-for-bit on disk, but no path now exists
to derive their plaintext.  This is the GDPR Art. 17
"crypto-shred" primitive; the audit-log entry is the
compliance artefact.  See the
[crypto-shred how-to](how-to/operating/crypto-shred.md)
and
[GDPR Art. 17 mapping](compliance/gdpr-art-17-crypto-shred.md).

#### What encryption algorithm does pg_hardstorage use?

AES-256-GCM-SIV (RFC 8452, nonce-misuse resistant) by
default.  AES-256-GCM with a random 96-bit nonce in FIPS
mode (BoringCrypto's FIPS module doesn't yet ship GCM-SIV
acceleration).  Per-chunk key derived as
`HKDF-SHA256(BDEK, info=chunk_hash)`; see
[envelope encryption](explanation/envelope-encryption.md).

#### Can I use my own per-tenant KEKs?

Yes — per-tenant KEK is a mandatory part of the
architecture.  Single-org users get a default tenant they
never see.  Multi-tenant SaaS configures one KEK per
customer; crypto-shred per customer is then a one-line
operation.  The reference is the encryption-plugin
contract:
[encryption contract](reference/plugins/encryption-contract.md).

#### Are chunks encrypted before they leave the agent?

Yes.  Encryption happens in-stream during the chunker
pipeline, before any byte hits the network.  S3 / GCS /
Azure see only ciphertext; the per-chunk SHA-256 in the
checksum header is over the plaintext (so dedup works
across compression and encryption settings without
re-uploading).

---

## Operations

#### How do I monitor backups?

Three layers: Prometheus metrics under the `pg_hardstorage_`
namespace, OpenTelemetry traces, and structured JSON
events delivered to configured Sinks (Slack, PagerDuty,
Jira, syslog, webhook…).  See
[monitoring](operations/monitoring.md) and
[alerting recipes](operations/alerting-recipes.md).

#### What does `pg_hardstorage doctor` check?

Reachability of PG, repo, and KMS; replication slot
health; last-backup age vs RPO target; retention applied;
schedule next-fire; disk space; FIPS mode; signature
verification on the most recent manifest; Patroni cluster
state when applicable.  Output is human-readable on TTY
and structured JSON otherwise.  `--fix` runs the
suggested commands after a confirmation prompt.  Reference:
[`doctor` CLI page](reference/cli/pg_hardstorage_doctor.md).

#### How do I schedule backups?

Either declarative SQL via CYBERTEC pg_timetable (the
recommended scheduler for fleet deployments), or the
built-in `pg_hardstorage schedule <deployment> "every 6 hours"`
which writes a systemd timer / K8s CronJob depending on
topology.  Recipe:
[schedule backups](how-to/operating/schedule-backups.md).

#### What's the dead-man's switch?

If no successful backup happens in `N×scheduled_interval`
(default `2×`), the control plane raises a `backup_overdue`
event through every configured Sink.  Likewise for WAL: if
no segment archives in `M minutes`, raise `wal_silence`.
Tunable per deployment.

#### How do I monitor WAL lag?

`pg_hardstorage_wal_archive_lag_seconds` and
`pg_hardstorage_wal_archive_lag_bytes` Prometheus metrics
plus the `wal.lag` doctor check.  Alerting before lag
approaches `max_slot_wal_keep_size` is the recipe in
[alerting recipes](operations/alerting-recipes.md).

#### Can I run a backup from a Patroni replica?

Yes — auto-selected at deployments ≥ 5 TB to keep primary
I/O free, and configurable explicitly via
`backup --from-replica`.  The agent uses Patroni's REST
API to find the lowest-lag replica and starts
`BASE_BACKUP` there.

#### How do I recover from "everything broke"?

The system ships seven named runbooks: R1–R7 cover repo-
region loss, KMS key destruction, cold-start from backups,
repo corruption at rest, half-applied PITR, slot dropped
with gap, and Patroni split-brain.  Each runbook is one
page, ends with a feedback link, and is versioned with the
binary.  Index:
[runbooks](reference/runbooks/index.md).

---

## Compliance

#### Does it produce SOC 2 evidence?

Yes — the audit log is hash-chained Merkle, periodically
anchored to a transparency log (Rekor by default,
customer-managed log on request).  `compliance report`
auto-generates a monthly PDF that maps observed events to
SOC 2 control IDs.  Mapping page:
[SOC 2 control mapping](compliance/soc2-control-mapping.md).

#### Is FIPS supported?

Yes, via the `pg-hardstorage-fips` build flavour
(`GOEXPERIMENT=boringcrypto`).  Refuses to start if
`crypto/tls` reports non-FIPS; the `--fips-strict` flag
panics on any non-FIPS plugin.  Recipe:
[FIPS variant](how-to/packaging/fips-variant.md).

#### How does GDPR Art. 17 crypto-shred work?

Each tenant has its own KEK.  Destroying the tenant's KEK
makes every backup wrapped under it mathematically
unrecoverable while leaving bytes on disk unchanged — the
audit chain entry is the compliance artefact.  One command:
`pg_hardstorage kms shred --confirm-keyring <keyring-dir> --reason "..."`.
See
[crypto-shred](how-to/operating/crypto-shred.md) and
[GDPR Art. 17 mapping](compliance/gdpr-art-17-crypto-shred.md).

#### Can I pin backups to a specific region?

Yes — per-deployment data residency policy: `regions: [eu]`
refuses any storage backend not in that list.  The check
runs at config-validate time and again at every PUT.  How-
to: [data residency](how-to/operating/data-residency.md);
compliance mapping:
[data residency pinning](compliance/data-residency-pinning.md).

#### What about HIPAA / PCI DSS / FedRAMP / ISO 27001?

Each has a dedicated mapping page that walks the relevant
controls and lists which feature satisfies each:
[HIPAA](compliance/hipaa.md),
[PCI DSS](compliance/pci-dss.md),
[FedRAMP](compliance/fedramp.md),
[ISO 27001](compliance/iso-27001-control-mapping.md).

#### How do I produce a signed evidence bundle for an audit?

`pg_hardstorage llm export-session <id>` for an LLM-
assisted operation, `pg_hardstorage audit export-bundle`
for the audit chain itself.  Both produce a tarball with
the events, the Merkle proof, the signing keys' public
material, and a cosign signature on the bundle.  See
[audit evidence bundles](compliance/audit-evidence-bundles.md).

#### Is there a legal-hold mechanism?

Yes — `pg_hardstorage hold add <deployment> <backup-id>`
suspends deletion regardless of retention policy.  The
hold is removable only by an actor with the right RBAC
verb, and every add/remove is audit-logged.  Recipe:
[legal hold](how-to/operating/legal-hold.md).

#### What's the n-of-m approval workflow?

Configurable per-operation threshold (e.g. require 2
approvers for `kms shred`, 3 for `repo wipe`).  The
`pg_hardstorage approval request` / `approval approve`
commands manage the workflow; once threshold is met, the
gated operation proceeds.  Recipe:
[n-of-m approvals](how-to/operating/n-of-m-approvals.md).

#### Does pg_hardstorage support WORM?

Yes — S3 Object Lock (Compliance mode), Azure immutable
blob, NetApp SnapLock, and a generic POSIX path
(`chattr +i`).  Configured per repo (`worm: true,
retention: 7y`), enforced via `SetRetention` on the
storage plugin contract.

---

## Plugin authoring

#### What's the difference between Tier-1 and Tier-2 plugins?

Tier-1 plugins are first-party, compiled into the binary,
self-register via `init()`, and ride the same signed
release.  Tier-2 plugins are third-party, ship as separate
binaries, and the agent discovers them on `$HSPLUGIN_PATH`
via `hashicorp/go-plugin` (gRPC over stdio).  Side-by-side
table:
[Tier-1 vs Tier-2](explanation/tier1-vs-tier2-plugins.md);
contract reference:
[plugin reference](reference/plugins/index.md).

#### Where do plugins live?

In-tree at `internal/plugin/<kind>/<name>/`, one
sub-directory per plugin.  Tier-2 plugins ship as standalone
binaries discoverable on `$HSPLUGIN_PATH`.  The
[storage plugin tutorial](tutorials/build-a-storage-plugin.md)
walks through writing one end-to-end.

#### Can I write a plugin in a language other than Go?

Yes — Tier-2 plugins use `hashicorp/go-plugin`'s gRPC
transport, so anything that speaks gRPC works.  The
[Tier-2 protocol reference](reference/plugins/tier2-go-plugin-protocol.md)
documents the wire format.

#### What plugin kinds exist?

Storage, Source, Encryption (KMS), Compression, Renderer,
Sink, and LLM provider.  Each is a small Go interface with
a defined contract; see
[plugin reference](reference/plugins/index.md).

#### Is there a public plugin registry?

Post-v1.0.  The plan is `registry.pghardstorage.org` with
cosign-verified plugin downloads.  Until then, distribute
Tier-2 plugins via your own channel; a checked-in
`$HSPLUGIN_PATH/<name>` directory is sufficient.

---

## Comparison

#### How is this different from pgBackRest?

The short version: WAL via replication protocol (works on
managed PG), no chained incrementals (CAS dedup with no
chain dependency), encryption / KMS / audit chain / WORM
on by default.  pgBackRest's strengths — production
maturity at very large scale, mature operator integrations
— are real and we're explicit about them.  Full comparison
with the cases each tool wins:
[vs pgBackRest, WAL-G, Barman](explanation/comparison-pgbackrest-walg-barman.md).

#### Can I migrate from pgBackRest?

Yes.  The migration runs side-by-side: pg_hardstorage
takes a fresh full backup while pgBackRest continues
serving restores from the legacy chain.  Once the new
repository has enough history to satisfy your recovery
window, retire pgBackRest.  Walk-through:
[from pgBackRest](how-to/migration/from-pgbackrest.md).

#### Can I migrate from WAL-G?

Yes — same shape: take a parallel full, optionally enable
the WAL-G CLI shim
(the `pg-hardstorage-walg` drop-in binary) so existing
automation keeps working during the transition, then
retire WAL-G.  Walk-through:
[from WAL-G](how-to/migration/from-walg.md).

#### Can I migrate from Barman?

Yes.  Barman's archive-command flow can be redirected to
`pg_hardstorage wal push` while a parallel streaming slot
catches up.  Walk-through:
[from Barman](how-to/migration/from-barman.md).

#### How is this different from a snapshot-only backup tool?

Snapshots are great for low-RPO local recovery but they
don't solve cross-region durability, WAL retention for
PITR, or compliance attestation.  pg_hardstorage uses
snapshots as one source-side input (auto-selected on COW
filesystems for ≥ 50 TB databases) and then carries the
content through the same chunked, encrypted, deduped repo
as the streaming path.

---

## Project & licensing

#### What's the license?

[Apache 2.0](https://www.apache.org/licenses/LICENSE-2.0).
No paid tier, no enterprise edition gate.  Compliance,
encryption, audit chain, FIPS, HSM, and the LLM helper are
all in the open-source build.

#### Who owns the project?

CYBERTEC PostgreSQL International GmbH.  Maintained as an
open-source project; contributions via DCO sign-off.  See
the [support page](support/index.md) for filing issues.

#### What Go version do I need?

Go 1.22+ for the default build.  The FIPS variant requires
Go 1.22+ on linux/amd64 with `GOEXPERIMENT=boringcrypto`.
The `Makefile` enforces these in CI.

#### Are releases signed?

Yes — every release artefact (tarball, `.deb`, `.rpm`,
container image) is cosign-signed and ships an SBOM
(syft-generated) plus an in-toto attestation built with
SLSA Level 3 provenance.  Verification:
[SLSA L3 provenance](compliance/slsa-l3-provenance.md).

#### How do I report a bug or vulnerability?

Routine bugs: file a GitHub issue (`gh issue create` or
the [repo issue tracker](https://github.com/cybertec-postgresql/pg_hardstorage/issues)).
Security vulnerabilities: see the disclosure policy linked
from the [support page](support/index.md).

#### Where does the documentation live?

In-tree under `docs/`.  The site is built with MkDocs
Material (`make docs-build`).  Contribution conventions
are documented in
[CONTRIBUTING-DOCS](CONTRIBUTING-DOCS.md); the IA decision
is in [DOC_PLAN](DOC_PLAN.md).

#### Is there an LLM helper?

Yes — `pg_hardstorage llm` is a grounded chat surface that
reads cluster state, runbooks, and audit log, and (in
opt-in `advise+execute` mode) can run pre-flight `--preview`
commands the operator confirms.  Read-only by default;
every suggestion is footnoted to its source tool call;
every session is hash-chained into the audit log.  See
[LLM safety stack](explanation/llm-safety-stack.md) and
the [LLM incident walkthrough tutorial](tutorials/llm-incident-walkthrough.md).
