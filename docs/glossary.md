---
title: Glossary
description: One-stop place to resolve any pg_hardstorage-specific term you encounter elsewhere in the docs.
tags:
  - glossary
  - reference
---

# Glossary

The terms a reader is likely to hit while learning
pg_hardstorage, in alphabetical order.  Each entry is one
to three sentences plus a "see also" link to the page that
goes deeper.  Cross-references between entries use the
exact term as it appears here.

If a term is missing, the
[support page](support/index.md) has the right place to
file the gap.

---

#### `advise+execute`

The opt-in privilege mode for the LLM helper that allows
`execute_command` after a successful `preview_command` in
the same turn, with explicit user confirmation.  Off by
default.  See the
[LLM safety stack](explanation/llm-safety-stack.md).

#### AES-256-GCM

The chunk-encryption cipher shipping today — a random
96-bit nonce per chunk.  AES-256-GCM-SIV (RFC 8452,
nonce-misuse-resistant) is the planned default once a
validated implementation lands; the Go standard library
does not yet ship it, and BoringCrypto (FIPS) does not
expose it either.  See
[envelope encryption](explanation/envelope-encryption.md).

#### Agent

The long-lived `pg_hardstorage agent` process that does
the actual work — base backup, WAL streaming, chunk
upload, manifest commit.  Co-located with the DB host or
remote, talking to PG over libpq.  One agent per host
multiplexes every deployment on that host.  See
[architecture tour § three execution modes](explanation/architecture-tour.md#1-three-execution-modes-one-binary).

#### `archive_command`

PG's per-segment WAL archive hook.  pg_hardstorage offers
a thin shim (`pg_hardstorage wal push %p`) for setups that
want classical archiving alongside streaming; both
shims feed the same content-addressed chunk store.  See
[wal pipeline](explanation/wal-pipeline.md).

#### `archive_library`

The PG 15+ replacement for `archive_command`, where a
shared library handles archive callbacks.
pg_hardstorage ships `pg_hardstorage_archive` (~200 LOC of
C) for the optional double-archiving path.

#### Attestation

An Ed25519-signed claim (agent keyring) attached to a backup
manifest, or a cosign-signed claim attached to a release
artefact, optionally anchored to a transparency log.  Both
backups and release binaries carry attestations.  See
[audit chain](explanation/audit-chain.md).

#### Audit chain

The append-only, hash-chained Merkle log of every
significant event (backup committed, restore started, KMS
rotated, LLM session opened, …).  Periodic anchors land in a
transparency log (self-hosted today; external Rekor anchoring
is roadmap).  Verifiable post-hoc via
`pg_hardstorage audit verify-chain`.  See
[audit chain](explanation/audit-chain.md).

#### Backpressure

The mechanism the chunker uses to keep slow stages from
buffer-bombing memory: each pipeline stage carries a
bounded `MemBudget`; if storage is slower than chunking,
the chunker blocks rather than allocating.  See the
resilience section of the
[architecture tour](explanation/architecture-tour.md).

#### Backup

One point-in-time recoverable artefact for a deployment —
a manifest plus the chunks and WAL it references.  See the
SPEC's
[Vocabulary table](explanation/architecture-tour.md).

#### BDEK

The "Backup Data Encryption Key" — a 256-bit random key
generated per backup, wrapped by the deployment's
[KEK](#kek), and stored in `manifest.json.encryption.wrapped_dek`.
See [envelope encryption](explanation/envelope-encryption.md).

#### `BASE_BACKUP`

The PG replication-protocol command pg_hardstorage uses to
take physical base backups.  Streams tar files per
tablespace; the agent feeds them through the chunker
pipeline.  Optional `INCREMENTAL <prior-manifest>` for
PG 17 incremental support.  See
[wal pipeline](explanation/wal-pipeline.md).

#### Cascading

A WAL streaming mode where Region-B's agent streams from
Region-A's repo (not from PG), keeping primary load
independent of region count.  Selected for multi-region
deployments.  See
[wal pipeline](explanation/wal-pipeline.md).

#### CAS (content-addressed store)

The repository layout in which every chunk is keyed by its
plaintext SHA-256.  Two backups that share a 4 KiB region
share one chunk; deleting one backup never invalidates
another.  See
[content-addressed storage](explanation/content-addressed-storage.md).

#### cgroup self-limit

The agent's startup hook that writes its own cgroup with
`memory.max` (default 70% of host) so approaching limits
triggers chunker backpressure rather than the kernel
OOM-killing us mid-`pg_backup_stop`.  Linux only.  See the
resilience section of the
[architecture tour](explanation/architecture-tour.md).

#### Chunk

A content-addressed unit of repository storage.  Plaintext
SHA-256 is the key; on-disk object is
`[1B version | 1B compression | 1B encryption | 12B nonce | payload]`.
See
[content-addressed storage](explanation/content-addressed-storage.md).

#### Circuit breaker

The per-backend-host failure-rate watchdog that opens (and
stops sending requests) on sustained errors, ramps back
up gradually after a cool-off.  Prevents a flaky region
from starving healthy ones.  See the resilience section of
the [architecture tour](explanation/architecture-tour.md).

#### Classification tag

A per-deployment label — Public / Internal / Confidential
/ Restricted — that drives retention floor, encryption
requirement, and allowed regions.  Set via
`pg_hardstorage classify`.  See
[data-residency how-to](how-to/operating/data-residency.md).

#### Control plane

The orchestration layer: schedules, RBAC, fleet view,
audit, verifier.  In-process for single-host (embedded
mode); a separate runtime for fleets.  Intentionally thin,
to avoid the central-throughput choke pgBackRest hits at
scale.  See
[three execution modes](explanation/three-execution-modes.md).

#### `coordination.k8s.io/Lease`

The Kubernetes-native lease primitive pg_hardstorage uses
for leader election in K8s topologies.  No second
coordination service needed.  See
[coordination without etcd](explanation/coordination-without-etcd.md).

#### cosign

The Sigstore CLI used to sign release artefacts (keyless).
Backup manifests are signed separately with the agent's
Ed25519 keyring, and that signature is verified on every
manifest read.

#### Cross-account replication

The async copy of every committed manifest + its chunks to
a repository owned by a different cloud account, with an
explicit ACL boundary.  Used for M&A and partner-data
scenarios.  See the SPEC's
[enterprise features § data lifecycle](https://github.com/cybertec-postgresql/pg_hardstorage/blob/main/SPEC.md).

#### DCS

"Distributed Configuration Store" — Patroni's term for
etcd / Consul / Zookeeper.  pg_hardstorage reuses
Patroni's existing DCS (writing under
`/pg_hardstorage/<deployment>/...`) instead of standing up
a second one.  See
[coordination without etcd](explanation/coordination-without-etcd.md).

#### Dead-man's switch

The control plane's overdue-backup alert: if no successful
backup happens in `N×scheduled_interval` (default `2×`),
raise `backup_overdue` through every configured Sink.
Same for WAL silence.

#### DEK

"Data Encryption Key."  In pg_hardstorage's three-layer
envelope, the DEK is the per-backup random key (the
[BDEK](#bdek)).  Chunks use further per-chunk keys derived
from the BDEK via HKDF.  See
[envelope encryption](explanation/envelope-encryption.md).

#### Deployment

A logical PostgreSQL service we back up — one Patroni
cluster, one standalone primary, one CNPG `Cluster`.  Replaces
the word *stanza* from pgBackRest.  Bound to ≥ 1 agents
for HA.

#### Docker sandbox

The default verifier sandbox: Docker `postgres:<major>`
container with tmpfs scratch, used by `verify --full` to
exercise restore + `pg_verifybackup` + `pg_amcheck`.  See
[verify-sandbox tradeoffs](explanation/verify-sandbox-tradeoffs.md).

#### Doctor

`pg_hardstorage doctor` — the single-command UX for "is
everything OK?".  Surfaces health checks per deployment
with plain-English remediation suggestions you run
yourself.  See the
[`doctor` CLI reference](reference/cli/pg_hardstorage_doctor.md).

#### Dual-slot / dual-stream

The WAL streaming mode where two replication slots on two
different nodes (typically primary + replica) feed the
same content-addressed store concurrently.  CAS dedup
makes duplicate chunks free; either stream can fail
without RPO impact.  Auto-selected at ≥ 50 TB or
`availability=high`.  See
[wal pipeline](explanation/wal-pipeline.md).

#### Embedded mode

The single-process execution mode where agent + minimal
control plane live in one binary, with bookkeeping in
small JSON files under `<state>/bookkeeping/`.  The
default for single-host deployments; restarting the binary
is the entire HA story.  See
[three execution modes](explanation/three-execution-modes.md).

#### Envelope encryption

The three-layer key hierarchy: a [KEK](#kek) wraps a
[BDEK](#bdek) which derives per-chunk keys via HKDF.
Crypto-shred works because destroying the KEK makes every
DEK unrecoverable.  See
[envelope encryption](explanation/envelope-encryption.md).

#### Evidence bundle

A signed, self-contained tarball produced by
`pg_hardstorage llm export-session` (or the audit
`export-bundle` command) containing every prompt, tool
call, response, executed command, and a Merkle proof.
Independently verifiable post-hoc; the artefact a regulator
sees in a post-incident review.  See
[audit evidence bundles](compliance/audit-evidence-bundles.md).

#### FastCDC

The content-defined chunking algorithm pg_hardstorage uses
(gear-hash, 4 KiB / 64 KiB / 256 KiB parameters), with
forced splits at PG's 8 KiB page boundaries for heap and
index files.  Keeps dedup ratios high across small page
changes.  See
[content-addressed storage](explanation/content-addressed-storage.md).

#### FIPS

The `pg-hardstorage-fips` build flavour
(`GOEXPERIMENT=boringcrypto`) — every `crypto/*` call
routes through Google's FIPS 140-2 validated module.
Refuses to start if `crypto/tls` reports non-FIPS.
`--fips-strict` panics on any non-FIPS plugin.  See
[FIPS variant](how-to/packaging/fips-variant.md).

#### Firecracker microVM

The opt-in alternative verifier sandbox — KVM-isolated
microVMs instead of Docker containers, for stronger
isolation when verifying untrusted backups.  Linux + KVM
only.  See
[firecracker sandbox](how-to/verify/firecracker-sandbox.md).

#### Tier-2 plugin transport

The wire contract pg_hardstorage uses to host Tier-2
plugins: a one-shot stdio JSON-RPC protocol
(`pg_hardstorage.plugin.v1`) — the host launches the plugin
executable per operation and exchanges line-delimited JSON
over stdin/stdout.  Crash-isolated, language-agnostic.  See
[Tier-2 protocol](reference/plugins/tier2-go-plugin-protocol.md).

#### `HSREPO`

The repo-root magic file (`{"version":1,"id":"...","tenants":[...]}`)
that marks a directory or bucket prefix as a
pg_hardstorage repository.  Read on every connect; refuses
to operate without it.

#### JIT access

"Just-in-time" — time-bound elevated tokens issued for
break-glass restore operations, auto-expiring, audit-
stamped.  Surfaced via `pg_hardstorage jit issue`.  See
the [`jit` CLI reference](reference/cli/pg_hardstorage_jit.md).

#### KEK

"Key Encryption Key."  The long-lived per-tenant key, held
in KMS, that wraps the [BDEK](#bdek) of every backup taken
under that tenant.  Destroying the KEK is the
crypto-shred primitive.  See
[envelope encryption](explanation/envelope-encryption.md).

#### KEKRef

The URL-shaped reference to a KEK
(`aws-kms://arn:...`, `gcp-kms://...`, `vault-transit://...`,
`local:default`, etc.).  Stored in
`manifest.json.encryption.kek_ref`.  Schemes documented in
the auto-generated KEKRef reference page.

#### Leader (Patroni)

The single primary in a Patroni-managed cluster, elected
via the DCS lease.  pg_hardstorage's agent watches the
Patroni REST API for leader changes and reconnects to the
new leader on failover.  See
[Patroni failover deep-dive](explanation/patroni-failover-deep-dive.md).

#### Legal hold

A flag that suspends deletion regardless of retention
policy — set via `pg_hardstorage hold add`, removable only
by an actor with the right RBAC verb, every change
audit-logged.  See
[legal hold](how-to/operating/legal-hold.md).

#### LLM helper

`pg_hardstorage llm` — the grounded chat surface that
reads cluster state, runbooks, and audit log; suggests
commands; (in opt-in mode) executes them after preview +
confirmation.  Read-only by default.  See
[LLM safety stack](explanation/llm-safety-stack.md).

#### LLMProvider

The plugin tier for chat-completion backends (OpenAI,
Bedrock, Vertex, Ollama, llama.cpp, Hugging Face, …).
Tier-1 in-tree, Tier-2 external via stdio JSON-RPC.  See the
[LLM provider contract](reference/plugins/llm-provider-contract.md).

#### Manifest

The per-backup JSON document — `manifest.json` — that
declares the backup's identity, LSNs, files, chunks, WAL
required, encryption envelope, and attestation.
Independently verifiable via the embedded public key.
Stored twice (canonical + replica prefix) for redundancy.

#### MCP server

`pg_hardstorage llm --mcp-server` — the Model Context
Protocol stdio (or TCP) server that exposes the LLM
helper's tool surface to any MCP-aware client (Continue,
Cursor, Zed, Goose, Cline, …).  See the SPEC's
[LLM helper § MCP](https://github.com/cybertec-postgresql/pg_hardstorage/blob/main/SPEC.md).

#### Merkle audit chain

See [audit chain](#audit-chain).

#### `--on-error-llm`

The CLI flag that auto-launches the LLM helper with the
failed command's context already loaded after a mutating
command fails.  The 3am-operator entry point.

#### `permanent_slots`

Patroni 3.0+'s replication-slot management feature.  When
a slot is declared as permanent, Patroni recreates it on
the new leader after failover with `restart_lsn`
propagated from the old leader.  pg_hardstorage's
preferred slot-continuity strategy.  See
[Patroni failover deep-dive](explanation/patroni-failover-deep-dive.md).

#### `pg_amcheck`

The PG-bundled tool that walks heap and index pages to
detect corruption.  Run as part of full-verify alongside
`pg_verifybackup`.

#### `pg_combinebackup`

The PG 17 tool that flattens an incremental chain into a
single full data dir.  pg_hardstorage's restore wraps it
when the selected target is an incremental.

#### `pg_rewind`

The PG-bundled tool a rejoining old primary uses to
fast-forward to a common ancestor LSN with the new primary
after a Patroni failover.  pg_hardstorage's slot-recovery
logic accounts for `pg_rewind` removing WAL from the
rewound node.

#### `pg_verifybackup`

The PG-bundled tool that re-checks file checksums against
a manifest.  Run after every restore (and as fast verify
after every backup).  Restore success is gated on this
unless `--skip-verify` is explicitly acknowledged.

#### `pg_walfile_name`

The PG SQL function that converts an LSN to a WAL filename.
Used by the backup orchestrator to confirm that the
`stop_lsn` of a base backup is in our WAL store before
manifest commit.

#### Page-aligned splits

The chunker's forced split at every 8 KiB page boundary
inside heap and index files (`base/`, indexes).  Keeps
dedup high: a single page changing in a 1 GB heap touches
exactly one chunk, not two.  See
[content-addressed storage](explanation/content-addressed-storage.md).

#### Patroni

The popular PG cluster-manager pg_hardstorage integrates
with via REST + DCS awareness, `permanent_slots`, dual-
slot, and sync-target modes.  See
[Patroni failover deep-dive](explanation/patroni-failover-deep-dive.md).

#### Plugin (Tier-1 / Tier-2)

A pluggable extension point for storage, source,
encryption, compression, renderer, sink, and LLM provider.
Tier-1 plugins are first-party and compiled into the
binary; Tier-2 plugins are third-party, ship as separate
binaries, and are discovered on `$HSPLUGIN_PATH` via
stdio JSON-RPC.  See
[Tier-1 vs Tier-2](explanation/tier1-vs-tier2-plugins.md).

#### Recovery slot

See [replication slot](#replication-slot).

#### Rekor

The Sigstore transparency log that pg_hardstorage anchors
its audit chain (and optional manifest signatures) into.
Public, verifiable, immutable.  See
[audit chain](explanation/audit-chain.md).

#### Renderer

The synchronous, command-scoped output plugin tier — takes
typed `Event` values and writes bytes to a `Writer`
(stdout, stderr, file).  Built-ins: `text`, `json`,
`ndjson`, `yaml`, `template`.  See the
[Renderer contract](reference/plugins/renderer-contract.md).

#### Replica (Patroni)

A non-primary PG node managed by Patroni.  pg_hardstorage
auto-routes backups to a Patroni replica at deployments
≥ 5 TB to keep primary I/O free.

#### Replica (manifest)

The redundant copy of a manifest written at
`manifests/_replicas/<id>.manifest.json`.  Cheap insurance
against single-key corruption.  Recoverable via
`pg_hardstorage repair manifest`.

#### Replication protocol

The PG protocol (`replication=database` connection
parameter) used for `BASE_BACKUP`, `START_REPLICATION`,
and `IDENTIFY_SYSTEM`.  pg_hardstorage's entire data plane
runs over this — the reason the agent needs no SSH or OS
access to the database host.  See
[wal pipeline](explanation/wal-pipeline.md).

#### Replication slot

A persistent server-side cursor that tells PG to retain
WAL until a specific consumer acknowledges it.
pg_hardstorage uses `pg_hardstorage_<deployment>` as the
default name.  See
[architecture tour § slot semantics](explanation/architecture-tour.md#2-wal-via-the-replication-protocol).

#### Repository (`repo`)

The destination where chunks, manifests, and WAL live —
e.g. `s3://acme-pg-backups/`.  One repo can hold many
deployments.  See the SPEC's
[Vocabulary table](explanation/architecture-tour.md).

#### RFC 5424

The syslog severity standard pg_hardstorage's event
severities map onto: emerg(0) / alert(1) / crit(2) /
error(3) / warning(4) / notice(5) / info(6) / debug(7).
Renderers and Sinks both use this mapping.

#### RKEK

"Repository Key Encryption Key" — a synonym for
[KEK](#kek) when emphasising that it's held at the
repository level.  See
[envelope encryption](explanation/envelope-encryption.md).

#### RPO

"Recovery Point Objective" — the maximum acceptable data
loss measured in time.  pg_hardstorage exports
`pg_hardstorage_rpo_seconds{deployment}` and surfaces SLO
violations through SLO-as-code.  See
[SLO as code](operations/slo-as-code.md).

#### RTO

"Recovery Time Objective" — the maximum acceptable
restore duration.  The agent runs a 30-second throughput
probe before a long restore and prints the projected RTO
in the preview block.

#### Runbook

A < 1-page Markdown playbook for a named disaster
scenario (R1–R7).  Shipped with the binary, addressable
from `doctor` when relevant, customisable per deployment.
See the [runbook index](reference/runbooks/index.md).

#### Scrub

The periodic chunk-rehash job (`pg_hardstorage repair
scrub`) that reads chunks back, decrypts and re-hashes
them, and surfaces mismatches as `verify.scrub_mismatch`.
Detects bit-rot at rest.  See
[scrub-and-heal how-to](how-to/operating/scrub-and-heal.md).

#### SCIM 2.0

The user / group provisioning standard pg_hardstorage
implements at v1.0 for auto-provisioning and
de-provisioning of human users from the IdP.

#### Severity

The RFC 5424-aligned numeric severity attached to every
`Event`.  Sinks declare a severity floor; the active
renderer doesn't filter (always renders what the user
asked the command to do).  See [RFC 5424](#rfc-5424).

#### Sidecar (mode)

The execution mode where the agent runs as a sidecar
container next to PG in the same Pod (CloudNativePG,
Zalando, Crunchy patterns).  Talks to PG over the local
Unix socket.  See
[three execution modes](explanation/three-execution-modes.md).

#### Sink

The asynchronous, system-scoped output plugin tier — takes
typed `Event` values and fans them out to external systems
on its own schedule (Slack, Jira, PagerDuty, syslog,
webhook, …).  See the
[Sink contract](reference/plugins/sink-contract.md).

#### Skill

A versioned, declarative YAML file under
`/usr/share/pg_hardstorage/skills/` that defines an LLM
behaviour (restore wizard, incident responder, …).
Hot-reloadable, signed, RBAC-scoped, golden-tested
against a pinned model checkpoint per release.  See the
SPEC's
[LLM helper § skills](https://github.com/cybertec-postgresql/pg_hardstorage/blob/main/SPEC.md).

#### SLO

"Service Level Objective" — a declarative per-deployment
RPO and RTO target.  pg_hardstorage compares observed
metrics against the SLO and raises alerts on violation.
See [SLO as code](operations/slo-as-code.md).

#### SLSA

"Supply chain Levels for Software Artifacts."
pg_hardstorage's release artefacts ship with SLSA Level 3
build provenance attestations.  See
[SLSA L3 provenance](compliance/slsa-l3-provenance.md).

#### Source

The plugin tier for backup sources — streaming
`BASE_BACKUP`, PG 17 incremental, snapshot
(ZFS / Btrfs / LVM / cloud-volume).  The agent picks a
source based on the deployment profile.  See the
[Source contract](reference/plugins/source-contract.md).

#### `START_REPLICATION`

The PG replication-protocol command that begins WAL
streaming from a slot at a given LSN.  pg_hardstorage
invokes it for every connection to the primary (or replica
in offload mode).

#### Stanza

pgBackRest's term for a logical PG service.  In
pg_hardstorage we use [Deployment](#deployment).

#### Storage URL scheme

The URL prefix that selects a storage backend —
`s3://`, `gcs://`, `azblob://`, `sftp://`, `scp://`,
`file://`, etc.  Auto-generated reference page lists all
built-in schemes.

#### Subject

The structured locator carried inside every `Event` —
`{Tenant, Deployment, BackupID, Timeline, …}` — used by
Sinks for ticket dedup (e.g. recurring failures update one
Jira ticket instead of spawning fifty).

#### Supervisor

The tiny parent process (< 5 MB RSS) that fork-execs the
agent worker, watches via Unix-socket heartbeat, captures
crash bundles on death.  systemd `Restart=always` is
layered on top for double-coverage.

#### Sync-target

The opt-in WAL mode (`wal_mode: synchronous`) where the
agent advertises itself as a `synchronous_standby_names`
candidate.  PG waits for our flush ACK before commit;
RPO = 0 at the cost of write latency.  See
[wal pipeline](explanation/wal-pipeline.md).

#### Tenant

An isolation boundary — one customer in SaaS, or a logical
zone like `prod`/`dev` in single-org.  Each tenant has its
own KEK; single-org users get a default tenant they never
see.  Crypto-shred operates at tenant granularity.

#### Tier-1 plugin

A first-party plugin compiled into the binary.  Self-
registers via `init()`; one signed binary is easier to
audit, FIPS-build, and ship.  See
[Tier-1 vs Tier-2](explanation/tier1-vs-tier2-plugins.md).

#### Tier-2 plugin

A third-party plugin shipped as a separate binary,
discovered on `$HSPLUGIN_PATH`, hosted via one-shot stdio
JSON-RPC.  Crash-isolated, language-agnostic.

#### Timeline

PG's monotonically-increasing identifier for a recovery
branch.  Promotion increments the TLI; any WAL the old
primary had not replicated is forked.  Manifests record
the timeline they ended on; PITR walks the timeline-
history files.  See
[Patroni failover deep-dive](explanation/patroni-failover-deep-dive.md).

#### Timeline history

The `.history` file PG produces on each promotion that
records the parent TLI and switch LSN.  pg_hardstorage
captures every `.history` file into the repo at first
sight; PITR across a failover boundary depends on it.

#### Transparency log

A public, append-only log (Rekor by default) that anchors
audit chain hashes externally so tampering becomes
detectable from outside the system being audited.  See
[audit chain](explanation/audit-chain.md).

#### TDE (Transparent Data Encryption)

Source-side encryption of PostgreSQL heap files, indexes,
the control file, and WAL at rest, performed by a PG fork
(CYBERTEC PGEE, `pg_tde`, EDB TDE) using a key the operator
holds.  Orthogonal to repo-side
[envelope encryption](explanation/envelope-encryption.md):
TDE protects bytes-at-rest on the SOURCE PG's disks, envelope
encryption protects bytes-at-rest in the REPO; both layers
can be active simultaneously.  pg_hardstorage handles TDE
deployments via a single per-deployment `tde.enabled: true`
flag — see [TDE awareness](explanation/tde-awareness.md).

#### TUI

The terminal user interface — `pg_hardstorage ui` (and the
LLM chat surface) — that renders fleet view, progress
bars, and chat conversations directly in the terminal.

#### Verifier

The subsystem that runs `pg_verifybackup` and (in full
mode) actually restores into a Docker / Firecracker /
k8s-Job sandbox to exercise restore.  See
[verify-sandbox tradeoffs](explanation/verify-sandbox-tradeoffs.md).

#### WAL

PostgreSQL's "Write-Ahead Log" — the durability primitive
that makes PITR possible.  pg_hardstorage streams WAL
continuously via the replication protocol and stores it as
content-addressed chunks alongside backups.

#### WAL gap

A discontinuity in the repo's WAL inventory between two
LSNs, typically caused by a slot drop or an asynchronous
failover.  The gap auditor detects gaps and emits
`wal.gap_detected`; the manifest of any backup taken after
a gap explicitly records the gap range; PITR inside the
gap window is refused with a clear error.  See
[wal pipeline § gap auditor](explanation/wal-pipeline.md).

#### `wal repair`

The explicit operator command that drops and recreates a
replication slot, accepting whatever WAL gap that
introduces (which it reports honestly).  Runbook:
[R6 — Slot dropped, gap detected](reference/runbooks/R6-slot-dropped-gap.md).

#### WORM

"Write Once, Read Many" — the immutability primitive
configured per-repo (`worm: true, retention: 7y`).  Backed
by S3 Object Lock (Compliance mode), Azure immutable blob,
NetApp SnapLock, or POSIX `chattr +i`.  Enforced via the
storage plugin's `SetRetention`.
