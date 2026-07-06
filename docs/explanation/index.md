---
title: Explanation
description: The "why" — conceptual deep-dives.
---

# Explanation

Conceptual deep-dives that explain **why** the system works the
way it does.  Read these when you want to build a mental model
rather than complete a task.

These pages are the canonical reference for design rationale.
Where they disagree with how-to guides, the how-to guide is
wrong; where they disagree with [`SPEC.md`](https://github.com/cybertec-postgresql/pg_hardstorage/blob/main/SPEC.md),
SPEC is the source of truth and we want to know — file an issue.

## Foundations

- [Design principles](design-principles.md) — the six rules that
  decide every architectural argument.
- [Architecture tour](architecture-tour.md) — the end-to-end
  picture: agent + control plane, three execution modes, plugin
  model.

## Data plane

- [WAL pipeline](wal-pipeline.md) — streaming as the data plane;
  single / replica-offload / dual / sync-target / cascading
  modes; the optional `archive_*` belt-and-suspenders paths.
- [Durability modes](durability-modes.md) — deferred chunk writes
  + one barrier per segment; `per-segment` vs `per-chunk`; why
  pg_hardstorage is an async archiver, not a synchronous standby.
- [Patroni failover deep-dive](patroni-failover-deep-dive.md) —
  the four cooperating mechanisms (REST awareness, slot
  continuity strategies A/B/C, dual-slot, sync-target) and
  timeline storage.
- [Content-addressed storage](content-addressed-storage.md) —
  FastCDC + page-aligned splits, plaintext SHA-256 keys,
  per-tenant fingerprint resistance.

## Security and compliance

- [Envelope encryption](envelope-encryption.md) — RKEK / BDEK /
  per-chunk Kc, AES-GCM-SIV vs AES-GCM-FIPS, KEK rotation flow.
- [TDE awareness](tde-awareness.md) — handling source PG with
  Transparent Data Encryption (CYBERTEC PGEE, pg_tde, EDB TDE):
  the one config flag, what does and doesn't change under TDE,
  the failure modes if it's forgotten.
- [Audit chain](audit-chain.md) — hash-chained Merkle audit log,
  transparency-log anchoring, what `audit verify-chain` actually
  checks.
- [LLM safety stack](llm-safety-stack.md) — the five gates
  (preview-required, replay-protected execute, typed
  confirmation, RBAC, n-of-m, anomaly refusal) plus signed
  evidence bundles.
- [Threat model](threat-model.md) — attacker capabilities the
  design defends against, and what is explicitly out of scope.

## Architecture

- [Three execution modes](three-execution-modes.md) — embedded,
  agent + control plane, sidecar; when to pick each.
- [Coordination without etcd](coordination-without-etcd.md) — the
  progressive ladder from JSON state files up to opt-in etcd.
- [Tier-1 vs Tier-2 plugins](tier1-vs-tier2-plugins.md) —
  compile-time vs stdio-JSON-RPC subprocess, trust posture, registry roadmap.
- [Verify-sandbox tradeoffs](verify-sandbox-tradeoffs.md) —
  Docker default vs Firecracker microVM; isolation vs setup
  cost.

## Comparison

- [pg_hardstorage vs pgBackRest, WAL-G, Barman]
  (comparison-pgbackrest-walg-barman.md) — honest comparison;
  what each tool does best; when to pick which.
