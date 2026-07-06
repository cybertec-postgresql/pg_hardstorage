---
title: pg_hardstorage vs pgBackRest, WAL-G, Barman
description: Honest comparison — what each tool does best, and the cases where pg_hardstorage actually wins on architecture, not on marketing.
tags:
  - comparison
  - evaluation
---

# pg_hardstorage vs pgBackRest, WAL-G, Barman

The first question every evaluator asks is "why not pgBackRest?"
This page is the honest answer.  We respect each of these tools;
they have all earned their production deployments many times
over.  This isn't a teardown — it's a clear-eyed comparison so
you can pick the right tool for your case.

The short version of where `pg_hardstorage` differs:

- **WAL via the replication protocol, not file-based archiving.**
  No SSH, no `archive_command`, and no extension on the database
  host — the agent backs up any self-managed PostgreSQL it can reach
  over the physical replication endpoint, from a different VM, region,
  or cluster.  (Fully-managed DBaaS such as RDS or Cloud SQL don't
  expose `BASE_BACKUP` and are out of scope.)
- **Content-addressed dedup with no chain dependency.**  Every
  backup is independently restorable; deleting one cannot break
  another.
- **Compliance posture (encryption / KMS / audit chain / WORM /
  FIPS) on by default.**
- **3 AM operator UX as a design principle**, not a side effect.

The places `pg_hardstorage` does *not* lead: **production
maturity at scale**.  pgBackRest in particular has had the better
part of two decades of refinement at very large fleets; WAL-G has
extensive cloud-storage tuning.  `pg_hardstorage` v1.0 will need
time to earn equivalent confidence.

---

## Quick table

|  | pg_hardstorage | pgBackRest | WAL-G | Barman |
| --- | --- | --- | --- | --- |
| **Primary WAL transport** | Replication protocol streaming | `archive_command` | `archive_command` | `archive_command` + streaming |
| **Backs up self-managed PG without host/SSH access?** | Yes (replication connection only) | No (needs SSH or archive_library) | No (archive_command on host) | Partial (streaming variant) |
| **Backs up managed DBaaS (RDS / Cloud SQL)?** | No (`BASE_BACKUP` not exposed) | No | No | No |
| **Backup chain model** | None (CAS, every backup independent) | Differential / incremental chained | Delta backups chained | Differential / incremental chained |
| **Dedup** | Cross-backup, cross-deployment, cross-tenant-ish | Within incremental chain | Page-delta | Within chain |
| **Encryption default** | AES-256-GCM envelope, on by default | Optional, configurable | Optional | Optional |
| **KMS support** | AWS / GCP / Azure / Vault / HSM | Per-deployment | AWS / GCP / Azure | Per-deployment |
| **Audit log** | Hash-chained Merkle, transparency-anchored (v0.5+) | Standard log file | Standard log file | Standard log file |
| **WORM** | First-class (S3 Object Lock, Azure immutable, NetApp SnapLock) | Backend-dependent | Backend-dependent | Backend-dependent |
| **FIPS build** | `pg-hardstorage-fips` flavour | Build-time | Build-time | Build-time |
| **Patroni integration** | REST-aware + permanent_slots + dual-slot + sync-target | Config integration | Patroni `bootstrap.method` | Standard PG replication |
| **K8s integration** | WAL-G shim, pgBackRest shim, Helm charts (CNPG-I provider on the v0.5 roadmap) | pgBackRest operators | WAL-G operators | Custom |
| **LLM helper** | First-class, audited, gated | n/a | n/a | n/a |
| **License** | Apache 2.0 | MIT | Apache 2.0 | GPL-3 |

---

## Where pgBackRest is the right answer

pgBackRest is excellent and we're comfortable saying so:

- **Battle-tested at scale.**  PostgreSQL deployments measuring in
  petabytes have used pgBackRest in production for years.  If
  "this exact configuration has been operated for a decade" is a
  decision-driver, pgBackRest leads.

- **Self-hosted PG with SSH access.**  pgBackRest's
  co-location-and-SSH model is genuinely simpler than the
  replication-protocol model when SSH is fine.  Fewer moving
  parts, fewer credentials.

- **Mature operator integrations.**  Crunchy Postgres for K8s
  and several other operator stacks default to pgBackRest.  If
  your platform team has already standardised, fighting the
  default isn't usually worth it.

The places to think twice:

- **You can't SSH into the host.**  When the backup tool can't
  get SSH/OS access to a self-managed primary (a locked-down VM,
  a host another team owns, a container you don't control),
  pgBackRest's co-location model is difficult; pg_hardstorage only
  needs a replication connection.  (Neither tool can back up a
  managed DBaaS like RDS — `BASE_BACKUP` isn't exposed there.)

- **You want backup chains that don't cascade.**  pgBackRest's
  incremental backup chain creates dependencies; one corrupt
  incremental can compromise the chain.  CAS backups don't have
  this property.

- **You want strong-default compliance posture.**  pgBackRest's
  encryption is opt-in; we want it on by default for every
  install.

---

## Where WAL-G is the right answer

WAL-G is a pure-storage-optimised tool that earns its keep:

- **You're already on a single major cloud and want
  storage-class-aware optimisation.**  WAL-G has years of
  refinement against S3 / GCS / Azure cost models.

- **You don't need a control plane.**  WAL-G is a CLI tool first;
  if you're orchestrating with your own scheduler, that's fine.

- **Delta backups for medium databases.**  WAL-G's page-level
  delta backups are well-tuned.

The places to think twice:

- **You want one tool from 10 GB to 100+ TB.**  WAL-G's posture is
  cloud-native single-node; scaling up to a large Patroni cluster
  changes the operational model.

- **You want centralised RBAC, audit, fleet view.**  WAL-G is a
  CLI; it doesn't ship a control plane.

- **You want `--preview` everywhere and a `doctor` command.**
  WAL-G's UX is aimed at automation, not interactive 3 AM use.

---

## Where Barman is the right answer

Barman is the canonical "Postgres-shop" backup tool in much of
the world:

- **You want a Barman server VM that owns backup state.**  This
  model is well-understood and many shops have institutional
  knowledge of it.

- **You're already using `barman cloud-*` to S3.**  Migration
  cost is non-trivial; Barman's cloud variant is solid.

- **You're paying for 2ndQuadrant / EDB support and Barman is
  what comes with it.**  Vendor support is a real
  decision-driver.

The places to think twice:

- **You want to avoid the Barman server VM as a single point of
  failure.**  `pg_hardstorage`'s agent-on-host (or sidecar) model
  removes the Barman-server bottleneck.

- **You want CAS dedup.**  Barman's incremental story is chain-
  based.

- **You want the first-class K8s integrations.**  Barman has
  K8s docs but isn't K8s-native in the way the operator-shim
  model expects.

---

## Where pg_hardstorage is the right answer

The cases where the architecture genuinely earns its
differentiation:

- **Self-managed PostgreSQL without host access.**  Backup runs
  over a replication connection, so the agent can live in a
  different VM, region, or K8s cluster from the database — no SSH,
  no `archive_command`, no extension on the host.  (Managed DBaaS
  like RDS or Cloud SQL are *not* supported: they don't expose
  `BASE_BACKUP`.)

- **Mixed fleet** of self-hosted + managed + K8s.  Same binary,
  same config schema, same repo schema.  The operator learns one
  tool.

- **Compliance-first posture.**  Encryption, KMS, audit chain,
  WORM, FIPS, crypto-shred all on by default.

- **3 AM operator UX as a feature**.  `--preview`, plain-English
  errors with suggested next commands, `doctor`'s per-issue
  remediation commands, the LLM helper.

- **No backup chain dependency**.  The "one corrupt incremental
  doesn't cascade" property is load-bearing for some compliance
  postures.

- **Patroni clusters** where you want the failover-aware slot
  story (REST + permanent_slots + dual-slot + sync-target) rather
  than rolling your own.

- **K8s with the operator-shim model**.  The WAL-G shim and
  pgBackRest shim let you swap into existing operator-managed
  clusters without rewriting the operator.  (A native CNPG-I
  provider is on the roadmap.)

- **Transparent Data Encryption (TDE) at the source.**  PG forks
  that encrypt heap / index / WAL at rest — CYBERTEC PGEE,
  `pg_tde`, EDB TDE — have one observable code path that fails
  against ciphertext: the `wal push` archive_command shim's
  segment-header read.  pg_hardstorage ships a single per-
  deployment `tde.enabled` flag that switches that path off, plus
  manifest stamping so restore-time tooling refuses vanilla-PG
  targets cleanly.  pgBackRest, WAL-G, and Barman have no
  equivalent — operators currently work around it with custom
  archive_command shapes.  See [TDE awareness](tde-awareness.md).

Where we are *honest* about being behind:

- **Production maturity.**  pgBackRest's decade-plus head start
  matters.  v1.0 is the start of `pg_hardstorage`'s production
  story, not the end.
- **Ecosystem of expert operators.**  More humans in the world
  know pgBackRest than know `pg_hardstorage` today.  Onboarding
  cost is real.

---

## A decision rubric

A simple rubric an evaluator can run:

1. **Is the PG you back up SSH-reachable?**  If no →
   `pg_hardstorage` (or Barman cloud variant).
2. **Do you have a fleet (5+ hosts) under one team?**  If yes →
   `pg_hardstorage` agent + control plane, or pgBackRest with
   careful repo-host sizing.
3. **Are you compliance-bound (PCI / HIPAA / FedRAMP / SOC2)?**
   If yes → all four can do it; `pg_hardstorage` makes the
   posture default; pgBackRest is the most-audited.
4. **Are you on Patroni and you've been bitten by failover slot
   gaps?**  If yes → `pg_hardstorage`'s four-mechanism story.
5. **Is your DB > 50 TB?**  If yes → `pg_hardstorage`'s
   dual-stream / snapshot story, or pgBackRest with parallel
   tuning.
6. **Are you already running pgBackRest happily?**  Stay there
   until you have a concrete reason to move.

---

## Further reading

- [Design principles](design-principles.md) — the choices that
  shaped this comparison.
- [WAL pipeline](wal-pipeline.md) — the
  replication-protocol-as-data-plane choice in depth.
- [Content-addressed storage](content-addressed-storage.md) —
  the no-chain-dependency property.
- [Architecture tour](architecture-tour.md) — the broader system
  this page compares against.
