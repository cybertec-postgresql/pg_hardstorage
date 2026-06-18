---
title: Backing up a Patroni cluster
description: Configure pg_hardstorage against a 3-node Patroni
              cluster — slot continuity across failover, leader-aware
              backup routing.
tags:
  - patroni
  - failover
  - cluster
---

# Backing up a Patroni cluster

> Wires `pg_hardstorage` to a 3-node Patroni cluster: one
> primary, two replicas, leader changes happen, the WAL stream
> continues without gaps. The agent watches Patroni's REST API,
> follows the leader, and recreates its replication slot on the new
> primary at the right LSN. About 25 minutes against a Docker-based
> Patroni stack.

The contract: the agent talks to one Patroni-managed PG cluster as a
single logical *deployment*. WAL streaming routes to the current
leader; on a leader change the streamer reconnects against the new
primary's slot at the saved LSN, and any cross-timeline gap is
recorded as a structured audit event so you can reason about RPO
afterwards.

---

## What you need

- A 3-node Patroni cluster reachable from the operator host. The
  `patroni/patroni` Docker examples or any of the published Compose
  stacks work; for a quick local stand-up, use [Patroni's reference
  stack](https://github.com/patroni/patroni/tree/master/docker).
- The Patroni REST URL — usually `http://patroni-leader:8008` for the
  reference Compose. Read endpoints (`/cluster`, `/leader`,
  `/history`) need no auth by default.
- A replication user (`CREATE ROLE pgbackup REPLICATION LOGIN ...`)
  with a `pg_hba.conf` line that admits the operator host on **every
  member**. Patroni's `bootstrap.pg_hba` block applies this fleet-wide.

---

## Steps

### 1. Confirm Patroni topology

Before any backup config, prove you can talk to Patroni:

```bash
# RUNNABLE skip-in-ci="needs Patroni cluster"
pg_hardstorage patroni status \
    --url http://patroni-leader:8008
```

```console
Cluster: pg-tutorial · scope=patroni-tutorial
  Member       Role      State    TLI  Lag    Host:Port
  patroni-1    Leader    running  3    0      10.0.0.11:5432
  patroni-2    Replica   running  3    24kB   10.0.0.12:5432
  patroni-3    Replica   running  3    8kB    10.0.0.13:5432
```

`patroni history` prints the timeline-history events that record
every promotion. The agent's leader-follow coordinator consumes those
events when it reconnects after a failover.

### 2. Initialise the repo and the deployment

```bash
# RUNNABLE skip-in-ci="needs Patroni cluster"
pg_hardstorage repo init file:///srv/hs-patroni-repo
```

For Patroni clusters, write the connection string against the
**Patroni REST URL's leader endpoint** rather than a single member —
the agent rewrites the host every time the leader moves. The simplest
form just points at the leader at config-time and lets the
follow-loop take over:

```bash
# RUNNABLE skip-in-ci="needs Patroni cluster"
pg_hardstorage init \
    --pg-connection 'postgres://pgbackup@patroni-leader.example.com/postgres' \
    --repo file:///srv/hs-patroni-repo \
    --deployment pg-tutorial \
    --skip-backup \
    --yes
```

### 3. Add the Patroni block to the deployment

`pg_hardstorage init` writes the deployment without Patroni
awareness. Either run

```bash
pg_hardstorage deployment edit pg-tutorial \
    --patroni-url http://patroni-leader:8008 \
    --patroni-slot pg_hardstorage_pg_tutorial \
    --patroni-interval 5s
```

…or hand-edit `pg_hardstorage.yaml` (path printed by
`pg_hardstorage doctor`) and add a `patroni:` block:

```yaml
deployments:
  pg-tutorial:
    pg_connection: postgres://pgbackup@patroni-leader.example.com/postgres
    repo: file:///srv/hs-patroni-repo
    patroni:
      url: http://patroni-leader:8008
      slot: pg_hardstorage_pg_tutorial   # explicit; default is auto-derived
      interval: 5s                        # poll cadence; matches Patroni's default leader TTL
      # user: opspg                       # optional HTTP basic auth
      # password: ...
```

`pg_hardstorage deployment list` shows the configured `patroni-url`,
`slot`, and `interval` so you can verify the block landed.

The `patroni.url` activates the agent's leader-follow coordinator.
With this block in place, every WAL streaming or backup run begins
with a Patroni leader probe and routes to the *current* primary —
not the host baked into `pg_connection`.

### 4. Start the agent (or run streaming directly)

For a long-lived setup you would run the agent under systemd:

```bash
pg_hardstorage agent
```

The agent spawns one Patroni follower coordinator per Patroni-enabled
deployment. A misconfigured Patroni URL on one deployment is
non-fatal — the follower for that deployment is dropped, the agent
emits `patroni.startup_partial`, and the rest of the fleet keeps
running.

For the tutorial, run the streamer directly so you can see the
events:

```bash
pg_hardstorage wal stream pg-tutorial \
    --pg-connection 'postgres://pgbackup@patroni-leader.example.com/postgres' \
    --repo file:///srv/hs-patroni-repo
```

### 5. Take a backup

```bash
# RUNNABLE skip-in-ci="needs Patroni cluster"
pg_hardstorage backup pg-tutorial \
    --pg-connection 'postgres://pgbackup@patroni-leader.example.com/postgres' \
    --repo file:///srv/hs-patroni-repo
```

For clusters at ≥ 5 TB the agent prefers a Patroni *replica* for the
base backup to keep primary I/O free. Replica preference is
automatic — there is no flag to flip.

### 6. Force a failover and watch the slot survive

In a Patroni-shell terminal, force a manual failover:

```bash
patronictl -c /etc/patroni.yml failover --master patroni-1 --candidate patroni-2
```

In a third terminal, follow the Patroni leader stream:

```bash
pg_hardstorage patroni follow \
    --url http://patroni-leader:8008
```

```console
2026-05-04T11:02:11Z  initial-leader  patroni-1 → 10.0.0.11:5432  TLI=3
2026-05-04T11:03:42Z  leader-change   patroni-1 → patroni-2       TLI=3 → 4
```

In the `wal stream` terminal you will see the agent reconnect:

```console
patroni.leader_change   from=patroni-1 to=patroni-2 new_lsn=0/A0000000
slot.repair             slot=pg_hardstorage_pg_tutorial result=created
wal.gap_recorded        range=[0/9F800000..0/A0000000]  cause=patroni-failover
wal.stream              receiving from patroni-2:5432
```

The agent recreated its replication slot on the new primary at the
saved LSN (or the lowest available LSN, whichever is more
conservative), and recorded the cross-timeline gap as a structured
audit event. You can list gaps later with:

```bash
pg_hardstorage wal gaps pg-tutorial \
    --repo file:///srv/hs-patroni-repo
```

### 7. Confirm continuity

`pg_hardstorage status` rolls up the picture:

```bash
pg_hardstorage status pg-tutorial
```

```console
pg-tutorial  PG 17.x  primary @ 10.0.0.12:5432  (Patroni leader)
  Last backup     8m ago  · full · 1.2 GB · ✓ verified
  WAL streaming   active   · lag 4s · slot pg_hardstorage_pg_tutorial
  Patroni         3 members · TLI 4 · last failover 2m ago
  RPO / RTO       8m / ~3m (estimate)
  Health          ✓ all clear
```

---

## What just happened

You configured one logical deployment against a real 3-node Patroni
cluster, exercised a failover, and watched the agent rebind to the
new primary without operator intervention. The two non-trivial pieces
of the design that earned their keep:

- **Patroni reuse, not duplication.** The agent reads Patroni's REST
  state and writes its own coordination keys under
  `/pg_hardstorage/<deployment>/...` in the existing DCS (etcd,
  Consul, ZooKeeper — whatever Patroni already runs). No second DCS,
  no coordination tax.
- **The slot is the source of truth.** PG retains WAL until the slot
  ACKs; the agent only ACKs after a segment commits in the repo.
  Combine that with the leader-follow coordinator and the gap window
  shrinks to "the seconds between the old primary's last commit and
  the new primary's promotion" — which is precisely the cross-
  timeline gap event the audit log records.

---

## Next steps

- [Kubernetes & CloudNativePG](kubernetes-cnpg.md) — same agent,
  per-Pod sidecar, CNPG-I provider.
- [R7 — Patroni split-brain](../reference/runbooks/R7-patroni-split-brain.md) —
  what to do when two members both think they are primary.
- [R6 — Slot dropped, gap detected](../reference/runbooks/R6-slot-dropped-gap.md) —
  diagnosing and repairing a WAL gap.
- [Operator guide — Restore](../operations/operator-guide.md#2-restore) —
  restoring from a Patroni-sourced backup is identical to a
  single-host one; PITR uses the same `--to` semantics.
