---
title: Scaling to large fleets
description: How pg_hardstorage stays fast and bounded across thousands of deployments, and the operator knobs that control it.
---

# Scaling to large fleets

`pg_hardstorage` is designed to back up thousands of PostgreSQL
deployments from a single control plane. This page explains the
mechanisms that keep the system fast and bounded at that scale —
and the knobs you tune for your fleet. None of them need any
configuration to be *correct*; they default to safe behaviour and
this page is about the *performance* envelope.

If you operate tens of deployments, you can skip this page: the
defaults are already comfortable.

## The shape of the problem

One control plane, one shared repository (object-store bucket),
and N agents — one per database host — that heartbeat and poll for
work. At N = 10,000 the naive version of several operations
becomes a problem:

- A single audit chain serializes every append behind one head
  pointer.
- Enumerating deployments by scanning every manifest is
  O(all backups in the fleet).
- Agents that started together poll in lockstep, hitting the
  control plane in synchronized bursts.
- A burst of queued jobs can claim and run unbounded concurrent
  backups, storming storage and the source databases.

Each is addressed below.

## Sharded audit chains

The tamper-evident audit log is partitioned into independent hash
chains — *shards* — keyed by the most specific scope an event
carries (deployment, then tenant, then a global chain for
repo-level events). Appends to different scopes never contend on a
shared head pointer, and each shard stays independently
verifiable.

This is transparent: nothing to configure, existing repos need no
migration, and the integrity guarantees are preserved (an event
can't move between shards without breaking its hash). See
[Audit chain → Sharded chains](../explanation/audit-chain.md#sharded-chains)
for the full design and the `audit verify-chain` behaviour.

## Fast deployment enumeration

Listing the fleet (`GET /v1/deployments`, and the many internal
callers — retention, GC, capacity, status) used to scan every
manifest object to recover the distinct deployment names —
O(total backups). It now reads a small per-deployment marker
index, so enumeration is O(number of deployments).

The index is maintained automatically at backup-commit time and
self-heals; an upgraded repo with no index is scanned once and the
index built in the background. Nothing to configure.

## Agent poll jitter

A fleet of agents started together — or restarted together after a
control-plane blip — would otherwise heartbeat (~10 s) and poll
(~5 s) in lockstep, hitting the control plane in synchronized
spikes. Each agent jitters its intervals by ±20% and spreads its
first tick across the whole interval, so requests arrive as a
smooth stream rather than a thundering herd. The jitter is
symmetric, so the mean request rate is unchanged.

This is on by default and needs no configuration. (Programmatic
embedders can tune `ControlPlaneClient.JitterFraction`; a negative
value disables it for deterministic load tests.)

## Job-concurrency cap (backpressure)

Without a limit, a burst of queued jobs — or a fleet all polling at
once — can claim and run an unbounded number of concurrent backups,
overwhelming the shared storage backend and the source PostgreSQL
instances.

The control plane can cap how many jobs run at once across the
whole fleet:

```sh
pg_hardstorage server \
    --repo s3://acme-pg-backups/ \
    --coord-backend pg --coord-dsn 'postgres://…' \
    --max-concurrent-jobs 200
```

Or in the server config file:

```yaml
server:
  max_concurrent_jobs: 200
```

Once the cap is reached, claims are refused and queued work stays
queued; agents keep polling and pick the work up as running jobs
complete and free a slot. **Zero (the default) means unlimited**,
so existing deployments are unchanged — opt in by setting a value
that matches your storage and database capacity.

Notes:

- For **multi-control-plane HA**, set the *same* value on every
  control plane. With the PostgreSQL coordination backend
  (`--coord-backend pg`) the cap is enforced globally over the
  shared jobs table.
- Both backends enforce a **hard** cap — exactly `n` jobs run at
  once, with no overshoot. The in-memory backend (single control
  plane) counts running jobs under the same lock as the claim; the
  PG path serializes the count-and-claim with a transaction-scoped
  advisory lock (a single global key, auto-released at commit), so
  independent control planes racing on separate connections still
  can't exceed the cap.

### Sizing the cap

Pick the cap from the bottleneck you want to protect:

- **Storage throughput / request limits.** If concurrent backups
  saturate the object store or hit per-account request limits,
  cap below that ceiling.
- **Source-database load.** Each backup is a `BASE_BACKUP` /
  WAL-stream against a primary (or replica). Cap so the aggregate
  doesn't degrade production.
- **Control-plane resources.** Progress streams and PG coordination
  connections scale with running jobs.

Start conservative and raise it while watching the
`pg_hardstorage_*` metrics and source-database load.

## Related

- One backup *per deployment* at a time is enforced separately by
  the per-deployment **backup lease** — a second concurrent backup
  of the same deployment is refused with
  `conflict.backup_in_progress`, independent of this fleet-wide cap.
- [Control-plane setup runbook](../reference/runbooks/control-plane-setup.md)
- [Capacity planning](capacity-planning.md)
- [Monitoring](monitoring.md)
