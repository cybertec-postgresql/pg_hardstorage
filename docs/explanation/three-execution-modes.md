---
title: Three execution modes
description: Embedded, agent + control plane, and sidecar — same binary, three deployment shapes, when to pick each.
tags:
  - architecture
  - deployment
  - kubernetes
---

# Three execution modes

`pg_hardstorage` is a single binary that runs in three different
deployment shapes.  Same code, same config schema, same wire
protocols — what changes is whether a control plane is in-process
or out-of-process, and whether the agent is co-located with PG or
remote.

This page is the conceptual map of which mode fits which
operational reality, and why we made the cut where we did.

---

## The three shapes

```mermaid
graph TB
    subgraph Embedded
        E[pg_hardstorage<br/>agent + minimal CP<br/>in one process]
        E -.libpq.-> EPG[(PostgreSQL)]
        E --> ESQL[SQLite-free<br/>JSON state files]
    end

    subgraph Agent + CP
        CP[Control plane<br/>schedules · RBAC · fleet view]
        A1[Agent A] -- mTLS --> CP
        A2[Agent B] -- mTLS --> CP
        A3[Agent C] -- mTLS --> CP
        A1 -.libpq.-> APG1[(PG host 1)]
        A2 -.libpq.-> APG2[(PG host 2)]
        A3 -.libpq.-> APG3[(PG host 3)]
    end

    subgraph Sidecar
        Pod[K8s Pod]
        Pod --> Sidecar[pg_hardstorage<br/>sidecar container]
        Pod --> PGC[postgres<br/>container]
        Sidecar -.unix socket.-> PGC
    end
```

| Mode | Agent | Control plane | Coordination | Best fit |
| --- | --- | --- | --- | --- |
| **Embedded** | In-process | In-process (minimal) | JSON state files | Single host, 1-N PGs |
| **Agent + CP** | One per host | Separate, HA-capable | PG advisory locks / etcd / K8s Lease | Fleet, 5+ hosts |
| **Sidecar** | One per Pod | Separate or one per cluster | K8s `coordination.k8s.io/Lease` | CNPG / Zalando / Crunchy |

The choice is operational, not architectural.  All three modes
read the same config, write to the same repo schema, and produce
restorable backups indistinguishable on disk.

---

## Embedded

The default when `init` doesn't see a control-plane URL.  One
binary as a systemd unit; the agent fork-execs a worker child
that does the heavy lifting; the parent watches via heartbeat.
"HA" is "the supervisor restarts the worker."

Bookkeeping lives in atomic-write JSON files under
`/var/lib/pg_hardstorage/bookkeeping/`.  No SQLite — we never
ship a non-PG database to manage backups of a database the
operator already runs.

**Pick this when:**

- One host, one PG.  The classic "I just want backups working" case.
- One host, many PGs.  Single agent multiplexes.
- Memory budget < 100 MB at idle is acceptable.
- You don't need fleet-wide views or RBAC.

**Don't pick this when:**

- Multiple hosts need centralised scheduling or RBAC.
- You want a fleet view across deployments.
- You need n-of-m approvals for destructive ops (these need the
  control-plane lease).

---

## Agent + control plane

Agents on each DB host (or remote, talking to PG over libpq).
Control plane somewhere reachable — a Linux VM, a K8s Deployment,
the same host in the smallest non-trivial case.  Identity is
`(host_fqdn, agent_uuid)`; agents register over mTLS; heartbeat
every 10 s.

The control plane orchestrates schedules, RBAC, the fleet view,
the verifier.  **It's intentionally thin** — orchestration only,
no heavy lifting.  The agent does the chunking, encryption, and
upload.  This avoids the central-throughput choke pgBackRest
hits at fleet scale.

**Pick this when:**

- 2+ hosts, one team operating them.
- You want one place to check whether every DB has a fresh
  backup.
- You want centralised RBAC, audit, and alerting.
- You need n-of-m approvals.

**Don't pick this when:**

- It's a single host.  Embedded is simpler and identical in
  behaviour for the single-host case.

The control plane runs HA via leader election in the
[coordination layer](coordination-without-etcd.md).  In
production it should run with at least two replicas.

---

## Sidecar

The agent runs as a sidecar container in the same Pod as PG,
talking over a Unix socket.  This is the canonical pattern for
CloudNativePG, Zalando Postgres Operator, and Crunchy PGO.

**Pick this when:**

- PostgreSQL itself runs in K8s.  This is the *only* sensible
  shape for that.
- You want backup posture changes to deploy via the standard
  Pod-template path.
- You're integrating with an existing operator (CNPG-I provider,
  WAL-G CLI shim, pgBackRest CLI shim).

The sidecar uses `coordination.k8s.io/Lease` for leader
election — no extra coordination service needed.

The K8s integrations specifically:

- **CloudNativePG** is supported today via the barman-cloud
  drop-in shim (`compat/barmancloud`); a native CNPG-I gRPC
  provider is still roadmap.
- **WAL-G drop-in shim** (`pg-hardstorage-walg` binary) for
  Zalando — shipped (v1.0.7).
- **pgBackRest drop-in shim** (`pg-hardstorage-pgbackrest`
  binary) for Crunchy PGO — shipped (v1.0.7).
- **Barman drop-in shim** (`pg-hardstorage-barman` +
  `pg-hardstorage-barman-wal-archive` binaries) for in-pod
  Barman wrappers — shipped (v1.0.7).
- **Helm charts**: `charts/pg-hardstorage-server` (control plane)
  and `charts/pg-hardstorage-sidecar` (per-Pod sidecar) — shipped.

The shim architecture changed between v0.5's plan
(`agent --*-shim` flag form) and v1.1's landing (segregated
drop-in binaries) — the segregation is deliberate; see
[compat/README.md](https://github.com/cybertec-postgresql/pg_hardstorage/tree/main/compat).

---

## Why the cut between embedded and agent+CP

The seam is at "do I need to coordinate across hosts?"  A single
host doesn't need leader election, doesn't need RBAC the
control plane provides, doesn't need a fleet view.  Forcing it
through a control-plane mode would mean spinning up a second
process and a coordination service for no operational benefit.

The flipside: embedded mode does *not* simulate the control
plane.  No fleet view, no per-deployment RBAC scopes beyond the
host's user, no centralised verifier scheduling.  These are
genuinely missing in embedded mode by design — promoting them
into embedded would force every single-host install to learn the
multi-host concept model.

When a single-host install grows into a multi-host fleet,
upgrading is straightforward: stand up a control plane, point
each agent at it with `pg_hardstorage agent register`, the
control plane discovers the existing deployments by listing
the repo, and embedded-mode bookkeeping migrates into the control
plane on first heartbeat.

---

## Why the agent is "fat" and the control plane is "thin"

The chunker, encryption, and storage upload are I/O- and
CPU-heavy.  Centralising them produces a chokepoint at fleet
scale — pgBackRest's repo-host model is exactly this, and at
50+ hosts it requires significant tuning to keep the repo host
from becoming the bottleneck.

`pg_hardstorage` puts the chunker / cipher / uploader on every
agent.  Each agent talks directly to the storage backend.  The
control plane orchestrates and observes; it does not touch
backup data.

Concrete consequence: doubling the number of hosts roughly
doubles the parallel ingest capacity to the storage backend.
There is no central component to scale.  The storage backend
itself (S3 / GCS / Azure / etc.) is engineered for parallel
ingest at this scale, so it's not the next bottleneck either.

---

## Further reading

- [Coordination without etcd](coordination-without-etcd.md) — how
  the three modes pick coordination backends without forcing a
  separate cluster service.
- [Architecture tour: three execution modes]
  (architecture-tour.md#1-three-execution-modes-one-binary) — the
  same material from the architecture-tour vantage.
- [Comparison vs pgBackRest, WAL-G, Barman]
  (comparison-pgbackrest-walg-barman.md) — the central-throughput
  point in context.
