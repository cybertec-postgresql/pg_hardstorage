---
title: Design principles
description: The six rules that decide every architectural argument in pg_hardstorage.
tags:
  - design
  - principles
  - resilience
---

# Design principles

Every backup tool inherits a thousand small decisions from the
posture it picks at the start.  This page is an honest articulation
of the posture `pg_hardstorage` picked, and the consequences that
fall out of each choice.  When two design alternatives appear
roughly equal in a code review, we re-read this page and pick the
one that aligns.

Six principles, in priority order.  Earlier principles dominate
later ones when they conflict.

---

## 1. Resilience above all

The reader most likely to open this binary's CLI is a tired,
stressed operator at 3 AM whose primary database is on fire.  A
backup tool that *fails strangely* under those conditions is worse
than no tool at all.  So every operation is built so that *the
failure modes are knowable*, recoverable, and explained.

What that means concretely:

- **Idempotency everywhere.**  Every action — chunk PUT, manifest
  commit, WAL segment archive, retention pass, repair operation —
  can be retried without changing the outcome.  Chunks are
  content-addressed and written `IfNotExists`.  Manifests commit via
  `RenameIfNotExists(.tmp → final)`.  Retention is computed from the
  state of the repo, not from a counter.  Crashing in the middle of
  any of these and re-running is safe.

- **No backup chain.**  An "incremental" is reduced to a list of
  content-addressed chunks the moment the chunker hashes the file.
  The manifest references chunks by hash; chunks present from any
  prior backup are reused without copying.  Deleting one backup
  cannot break another.  This is the single biggest behavioural
  difference from chain-based tools like pgBackRest or WAL-G.

- **Crash-only design.**  No graceful shutdown is required for
  correctness.  On startup the agent reads `state/inflight.json`,
  reconciles it against the repo, releases server-side state
  (`pg_backup_stop(false)` if a backup was open), and continues.
  Power loss mid-operation is just another restart.

- **Pre-flight checks block destructive operations.**  Restore,
  delete, `kms shred`, `repo gc` all run a checklist before they
  will accept `--yes`.  See the [restore preview]
  (../how-to/index.md) for the operator-facing form.

- **Plain-English errors with a "next step" suggestion baked into
  every failure.**  Every `Error` value carries a `Suggestion`
  field.  The CLI prints it; the API exposes it; structured logs
  include it.  No user-visible failure says only "ERROR: nil
  pointer".

Read the deeper treatment of these in
[the resilience chapter of the architecture tour](architecture-tour.md#8-resilience-design)
and the [repair toolkit reference](../reference/runbooks/index.md).

---

## 2. Compliance is a feature, not a tier

`pg_hardstorage` is Apache 2.0.  There is no paid edition.
Encryption, KMS integration, audit log, signed manifests, WORM,
FIPS, and crypto-shred are *on by default* in the open-source
binary — not held back behind a license check.

We picked this for two reasons.  First, "compliance as upsell" has
historically meant that the most security-critical defaults are
the *worst* ones in the free tier; we refuse to ship that posture.
Second, audit logs and signed manifests are how the system stays
honest with its own operators — we want every install, including
homelabs, to have them.

Concrete consequences:

- The cipher default is AES-256-GCM-SIV (RFC 8452, nonce-misuse
  resistant).  AES-256-GCM with a random 96-bit nonce is the FIPS
  fallback when `BoringCrypto` is in use.  See [envelope
  encryption](envelope-encryption.md).

- The audit log is hash-chained on every write — no opt-in, no
  feature flag.  The chain is verifiable post-hoc with `audit
  verify-chain`.  See [the audit chain](audit-chain.md).

- WORM (Object Lock / immutable blob) is a per-deployment flag,
  not a tier.

- Per-tenant KEKs are mandatory architecture even for single-org
  installs (you get a default tenant you never see).  This makes
  GDPR crypto-shred a one-line operation.

The cost is non-trivial: every commit pays for an HMAC chain step
and a wrap/unwrap round-trip.  We accept that cost as the price of
the posture.

---

## 3. Simplicity is the headline product

The 3 AM operator must succeed *without reading the docs first*.
The doctrine that follows from that is: **common workflows are one
command, defaults are correct, and the binary ships everything it
needs to be useful.**

This shapes the surface in some specific ways:

- `pg_hardstorage init` is a single interactive wizard that takes
  the operator from zero to a working backup of their first
  database in under five minutes.  It picks a sensible chunker
  size, a sensible retention (GFS), a sensible verification cadence,
  and a sensible KEK (passphrase if you didn't configure KMS).

- Retention, verification, scheduling, alerting, and self-diagnosis
  are *built-in* — not bolted on, not "see also pg_timetable + cron".
  Schedule is `pg_hardstorage schedule db1 "every 6 hours"`.  Alerts
  are `pg_hardstorage notify add slack <webhook>`.  Self-diagnosis is
  `pg_hardstorage doctor`.

- The vocabulary is plain.  *Deployment* (not stanza), *backup*,
  *restore*, *repository*.  No jargon that requires the operator to
  learn the tool's metaphor before they can talk about their
  database.

- The most useful command is `doctor`, and we expect operators to
  run it whenever something feels off.  Each finding includes a
  remediation suggestion, and `doctor` prints the exact command
  to run — the operator applies it after reviewing.

- Every mutating command supports `--preview` and `--dry-run`.

Simplicity does *not* mean "few features."  The feature surface is
large because real backup operations are large.  Simplicity here
means **that surface is discoverable and the defaults are right**.

---

## 4. Scale-spanning

Same binary, same UX, same config file from a 10 GB single-host
PostgreSQL to a 100+ TB Patroni cluster on Kubernetes.  Big-database
features (parallel chunk pipeline, snapshot base backups, multiple
WAL streams from replicas) are *automatic upgrades* the system
chooses based on database size and topology — they are **not** a
separate "enterprise mode" the operator has to opt into.

The size-tiered defaults table:

| Database size | Default base-backup strategy | Default WAL transport | Chunker concurrency |
| --- | --- | --- | --- |
| < 100 GB | streaming (replication-protocol BASE_BACKUP) | streaming | 4 workers |
| 100 GB – 5 TB | streaming + parallel file uploaders | streaming | 16 workers |
| 5 – 50 TB | streaming **from a Patroni replica** | streaming + redundant replica feed | 32–64 workers |
| 50 – 100+ TB | **snapshot** (ZFS / Btrfs / LVM / cloud volume) | streaming, **two slots** (primary + replica) | 64–128 workers |

The agent picks the row by inspecting the database; `doctor`
reports which row is active.  No `--enterprise` flag exists.

This is also the principle that motivates the [embedded /
agent+CP / sidecar split](three-execution-modes.md) — same binary,
three deployment shapes — and the [coordination ladder]
(coordination-without-etcd.md) — JSON files for one host,
Postgres advisory locks for small fleets, K8s Lease for K8s, etcd
only when you really need it.

---

## 5. WAL via the replication protocol, not URLs

The default WAL transport is `START_REPLICATION` over libpq, fed
into a persistent physical replication slot.  Not `archive_command`
spawning a wrapper script.  Not a polling loop on `pg_wal/`.  Not
SSH into the host to read files.

The reasoning is a single sentence: **we want the agent to work
where SSH and `archive_library` cannot reach.**  Managed PostgreSQL
on AWS RDS, GCP Cloud SQL, Azure Database, Aiven, Supabase, Neon —
none of them let you install a C extension, none of them let you
SSH into the host.  All of them expose the standard PostgreSQL
replication endpoint.  Streaming over the replication protocol
works on every one of them, identically to a self-hosted PG.

Bonus consequence: a persistent slot **closes the WAL gap window
to zero by default**.  PG holds WAL on disk until our agent ACKs.
A network blip up to ~10 s is tolerated transparently.

The C `archive_library` extension and `archive_command` shim are
*optional belt-and-suspenders* for environments where customers
want classical archive paths alongside streaming.  Both feed the
same content-addressed chunk store; CAS dedup makes the duplicate
writes free.

The deeper tour of the WAL pipeline is in
[wal-pipeline.md](wal-pipeline.md).

---

## 6. No magic strings, no jargon

Backup tools accumulate vocabulary the way ships accumulate
barnacles.  We say *deployment* and *backup* and *restore*.  Not
"stanza", not "repo node", not "diff backup".  The command tree
reads like English: `pg_hardstorage backup db1`,
`pg_hardstorage restore db1 --to "5 minutes ago"`,
`pg_hardstorage doctor db1 --fix`.

This sounds like aesthetics; it isn't.  It's a forcing function for
the design.  When a feature can't be named in plain English, it's a
sign the underlying mental model is wrong.  Several proposed
features were redesigned during early reviews because we couldn't
explain what they did without inventing a noun.

The flip side: where PostgreSQL itself has an unavoidable term of
art (LSN, timeline, slot, system identifier), we use the
PostgreSQL term unchanged.  No new "pg_hardstorage LSN" — that's
just the LSN.

---

## How these principles trade off

The principles are listed in priority order.  When two collide,
the higher-numbered one yields.  Some real examples:

- **Resilience vs simplicity:** the manifest is written *twice* —
  once at the canonical path, once at a replica prefix.  That's a
  resilience feature that costs the simplicity of "one manifest
  file per backup".  Resilience wins.

- **Compliance vs scale:** every chunk PUT carries a SHA-256
  checksum header.  At 100 TB scale that's millions of small
  hash computations.  Compliance wins; we engineer around the
  cost (parallel chunker, mmap'd buffers, adaptive concurrency).

- **Simplicity vs scale-spanning:** auto-selection of dual-slot
  WAL streaming at 50 TB+ is *not* visible in the config file.  It
  shows up in `doctor` and `status` but the operator doesn't have to
  understand it to run a backup.  Scale-spanning wins; the
  complexity is hidden.

- **Compliance vs WAL via replication protocol:** what about FIPS?
  AES-256-GCM-SIV isn't FIPS.  We ship a `pg-hardstorage-fips`
  build with `GOEXPERIMENT=boringcrypto` that falls back to plain
  AES-256-GCM with a random 96-bit nonce.  Both principles are
  served — at the cost of two build flavours.

The principles are not aspirational.  Every one of them is
load-bearing in a specific subsystem; if you propose changing one,
expect to also propose what changes downstream.

---

## Further reading

- [Architecture tour](architecture-tour.md) — how the principles
  surface in the shipped system.
- [WAL pipeline](wal-pipeline.md) — principle 5 in the
  ground truth.
- [Envelope encryption](envelope-encryption.md) — principle 2 in
  cipher form.
- [Audit chain](audit-chain.md) — principle 2 in tamper-evidence
  form.
- [Threat model](threat-model.md) — what the principles together
  defend against, and what they don't.
