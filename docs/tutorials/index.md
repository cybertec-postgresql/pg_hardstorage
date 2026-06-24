---
title: Tutorials
description: Learn-by-doing — start here if you're new to pg_hardstorage.
---

# Tutorials

Learn-by-doing, start to finish. Each tutorial takes you through a
real task on a working system; follow them in order if you're new,
or jump to the one that matches what you're trying to learn.

If you just want to get a backup running in five minutes, start
with the simple quick-start; if you want the full CLI picture,
take the full getting-started instead.

## Getting started

- [Getting started (simple)](getting-started-simple.md) — the
  interactive `pg_hardstorage_simple` helper; a working backup in
  five minutes, no config file to write.
- [Getting started (full CLI)](getting-started.md) — the complete
  `init` → `backup` → `status` → `restore` walkthrough against a
  local PostgreSQL.
- [First backup and restore](first-backup-restore.md) — take a
  base backup, inspect the manifest, and restore it into a sandbox.

## Core workflows

- [Point-in-time recovery](pitr-tutorial.md) — drive WAL replay to
  a wall-clock time, an LSN, or a named restore point.
- [Envelope encryption — local KEK and AWS KMS](encryption-walkthrough.md)
  — turn on encryption with a passphrase-wrapped local key, then
  graduate to AWS KMS.
- [LLM incident walkthrough](llm-incident-walkthrough.md) — use the
  grounded LLM helper to triage a failed restore at 3am.

## Topologies

- [Backing up a Patroni cluster](patroni-cluster.md) — leader-aware
  streaming, slot continuity, and failover-safe backups.
- [Kubernetes with CloudNativePG](kubernetes-cnpg.md) — back up and
  restore a CNPG cluster with the CronJob / Deployment model.

## Extending pg_hardstorage

- [Build a Tier-2 storage plugin](build-a-storage-plugin.md) — author
  an out-of-tree storage backend over the `go-plugin` contract.
