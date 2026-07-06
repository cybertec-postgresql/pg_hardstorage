---
# `title` drives only the <title>/browser-tab text, not the on-page
# H1.  It must NOT repeat the site_name ("pg_hardstorage"), or
# Material renders the duplicate "pg_hardstorage - pg_hardstorage".
# Keep it descriptive + keyword-front-loaded for SEO; Material
# appends " - pg_hardstorage" automatically.
title: PostgreSQL backup, WAL streaming & PITR
description: >-
  pg_hardstorage is an open-source PostgreSQL backup agent and CLI:
  continuous WAL streaming, point-in-time recovery, content-addressed
  deduplication, envelope encryption, and signed manifests. PG 15–18,
  Apache 2.0.
---

# pg_hardstorage

<p class="hero-logo" markdown>
![pg_hardstorage](assets/pghardstorage_logo_dark.png){.logo-on-light}
![pg_hardstorage](assets/pghardstorage_logo_light.png){.logo-on-dark}
</p>

> PostgreSQL backup, done right — agent + CLI for
> resilient, compliant, content-addressed backup with native
> WAL streaming, envelope encryption, and a built-in
> [restore-time LLM helper](llm-helper.md).

`pg_hardstorage` is a single Go binary that doubles as a
long-running host agent and an interactive CLI.  It ferries
base backups and WAL via the **PostgreSQL replication
protocol** (so it needs no SSH, OS access, or `archive_command`
on the database host), deduplicates
content-addressed chunks, AES-256-GCM-encrypts under a
per-backup DEK wrapped by a configurable KEK, and signs
every manifest with Ed25519.

PG 15–18, Apache 2.0.  Targets single-host deployments,
multi-tenant SaaS, and Patroni clusters with the same
binary.

## The shape of a deployment

Two `pg_hardstorage` processes run side by side against the
same repo:

- **`pg_hardstorage wal stream <deployment>`** — the always-on
  data plane.  Holds a physical replication slot, ships
  every completed 16 MiB WAL segment into the repo as
  PostgreSQL fills it, never stops.  This is the headline
  feature.  (Bytes inside a partial segment are received
  but not yet durable; PG retransmits them on reconnect, so
  the on-disk repo is gap-free at segment granularity.)
- **`pg_hardstorage backup <deployment>`** — runs on a
  schedule (e.g. nightly).  Streams a base backup over the
  replication protocol while the WAL stream keeps running
  concurrently.

The streamer's slot pins `restart_lsn` on the server, so
PostgreSQL refuses to recycle WAL until the agent ACKs.  A
crash of the streamer is just a restart with no gaps.  PITR
is byte-precise: pick any backup, replay WAL from the same
repo up to the target LSN/time.

---

## Pick your starting point

| You're a... | Start here |
| --- | --- |
| New user, want to take a backup in 5 minutes | [Getting started](tutorials/getting-started.md) |
| Operator with a running deployment | [Operator guide](operations/operator-guide.md) |
| Tier-2 plugin author | [Tier-1 vs Tier-2 plugins](explanation/tier1-vs-tier2-plugins.md) · [Build a storage plugin](tutorials/build-a-storage-plugin.md) |
| Compliance auditor | [Audit-log architecture](explanation/architecture-tour.md) |
| 3am on-call, restoring right now | [Runbooks](reference/runbooks/index.md) |
| K8s operator team integrating CNPG / Zalando / Crunchy | [Kubernetes how-to guides](how-to/kubernetes/index.md) |

---

## Documentation map

The site follows the [Diátaxis](https://diataxis.fr/)
quadrant model — every page is one of:

- **Tutorial** — *learn-by-doing*, fixed paths, end-to-end
- **How-to** — *task-oriented*, recipes for one specific
  problem
- **Reference** — *exhaustive technical specs*, machine-
  comparable, often auto-generated
- **Explanation** — *the "why"*, conceptual deep-dives

Plus operations (day-2 handbook), compliance (control
mappings), and runbooks (the 3am on-call's friend).

The roadmap, decisions, and authoring conventions live in
the [Documentation plan](DOC_PLAN.md).

---

## Project status

All advertised v1.0 features are implemented; the
documentation effort tracked in this site is the v1.0
finishing pass.  See the [release notes](release-notes/index.md)
and the [changelog](changelog.md) for release-by-release
detail.
