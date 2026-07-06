# Architecture

This document explains how `pg_hardstorage` is built — for the
engineer evaluating whether it fits, the operator who needs to reason
about failure modes, or the contributor extending it.

The full design specification lives at
[`SPEC.md`](https://github.com/cybertec-postgresql/pg_hardstorage/blob/main/SPEC.md)
in the repo root.  What follows summarises the architecture as it
stands at v0.1.

---

## 1. Three execution modes, one binary

Same binary, three deployment shapes:

- **Embedded** — single process, agent + minimal control plane in one
  process, talking to local SQLite for inflight state. The default
  for a single host backing up one or a few databases. Restarting the
  binary is the entire HA story.
- **Agent + control plane** — agents on each DB host (or remote, via
  libpq), control plane somewhere reachable. Agents register over
  mTLS, identity is `(host_fqdn, agent_uuid)`, heartbeat every 10 s.
  Control plane orchestrates schedules, RBAC, fleet view, verifier.
- **Sidecar** — agent runs as a sidecar container next to PG in the
  same Pod (CloudNativePG, Zalando, Crunchy patterns). Talks to PG
  over the local Unix socket.

The agent is intentionally fat (does the heavy lifting); the control
plane is intentionally thin (orchestrates). This avoids the central
throughput choke pgBackRest hits at fleet scale.

---

## 2. WAL via the replication protocol

The whole data plane is built around the PostgreSQL replication
protocol over a normal database connection. Base backup uses
`BASE_BACKUP`. WAL flow uses `START_REPLICATION SLOT ... PHYSICAL`.
Logical decoding (shipped) uses `START_REPLICATION SLOT ... LOGICAL`.
One connection model, one credential, one mental model.

### Why

The agent needs no OS-level access to the database host. It can be in
a different VM, different region, different cloud, different K8s
cluster. It works against any self-managed PostgreSQL that exposes the
physical replication endpoint. This is the largest single
architectural difference from pgBackRest, which assumes co-location
and SSH.

Fully-managed DBaaS — AWS RDS, Aurora, GCP Cloud SQL, Azure Database,
Neon, Supabase — do **not** expose `BASE_BACKUP` or physical
replication to customers, so they are out of scope: pg_hardstorage
cannot take a physical base backup of them.

### How

A persistent physical replication slot per deployment, named
`pg_hardstorage_<deployment>`, created on first run with
`CREATE_REPLICATION_SLOT ... PHYSICAL RESERVE_WAL`. PG then holds WAL
on disk until our agent ACKs (`Standby Status Update`). We send
keepalives every 5 s, well under PG's `wal_sender_timeout` default of
60 s. A tolerable network blip is ~10 s.

WAL records flow as soon as PG flushes. The agent assembles
16 MiB segments in memory, runs them through the FastCDC chunker, and
commits per-segment manifests atomically.

### Slot semantics

The slot is **permanent** in PG's sense — `pg_hardstorage` does not
drop it across normal restarts. Killing the streamer for a long time
WILL bloat `pg_wal/` on the primary; that is the cost of the
no-gap guarantee. `wal repair` is the explicit operator action that
drops and recreates the slot, accepting whatever WAL gap that
introduces (which the gap auditor reports).

Multi-node Patroni clusters have richer slot stories — see Patroni
section below.

---

## 3. CAS chunk store

The repo is a content-addressed store. Every chunk is keyed by its
plaintext SHA-256. Two backups that share a 4 KiB region share one
chunk. This is also how restores from incrementals work — there is
no chain, every backup's manifest references all the chunks it needs,
and chunks present from any prior backup are simply re-used.

### FastCDC + page-aligned splits

Chunking uses FastCDC (gear-hash, 4 KiB / 64 KiB / 256 KiB
parameters) with **forced splits at PG's 8 KiB page boundaries** for
heap and index files. The forced splits keep dedup ratios high across
backup-to-backup churn: a single page changing in a 1 GB heap touches
exactly one chunk, not two.

### SHA-256 keys, not hashes-of-ciphertext

The chunk key is the plaintext SHA-256, not the ciphertext SHA-256.
This means dedup-within-key works across compression posture and
encryption setting — encrypting a previously-unencrypted backup
doesn't double-store anything; changing zstd level doesn't break dedup.

### On-disk envelope

Each chunk is stored as:

```
[1B version=0x02][1B compression-algo][1B encryption-algo][12B nonce][payload]
```

Self-describing. v0.1 readers handle v0x01 (legacy pre-encryption) and
v0x02 transparently. Compression algos: `zstd`, `none`. Encryption
algos: `aes-256-gcm`, `none`.

### Atomic writes

`Put` uses `O_CREATE|O_EXCL` (or S3 `If-None-Match: *`); a retried
upload is a no-op. Manifest commits use `RenameIfNotExists` (POSIX
`link(2)` + `unlink(2)`, or S3 conditional rename). Either fully
visible or never visible.

---

## 4. Manifest schema

Per-backup JSON, signed with Ed25519. The public key is embedded so a
manifest is independently verifiable without external state.

```json
{
  "schema": "pg_hardstorage.manifest.v1",
  "manifest_version": 1,
  "backup_id": "db1.full.20260427T093017Z",
  "deployment": "db1",
  "type": "full",
  "pg_version": 170,
  "system_identifier": "7388123...",
  "start_lsn": "0/3000028",
  "stop_lsn":  "0/30001A0",
  "timeline":  1,
  "compression": "zstd",
  "encryption": {
    "scheme":           "aes-256-gcm",
    "wrapped_dek":      "base64...",
    "kek_ref":          "local:default",
    "envelope_version": 2
  },
  "tablespaces": [{"oid":1663,"location":"pg_default"}],
  "files": [
    {"path":"base/16384/2619","size":8192,
     "chunks":[{"hash":"aabb...","offset":0,"len":4096},
               {"hash":"eeff...","offset":4096,"len":4096}]}
  ],
  "wal_required": ["000000010000000000000003"],
  "attestation": {"sig":"...", "public_key":"..."}
}
```

### Redundancy

Every commit writes the manifest twice — once at the canonical path
(`manifests/<deployment>/backups/<id>/manifest.json`) and once at a
replica prefix (`manifests/_replicas/<id>.manifest.json`). Replica
write failure is reported via a warning event (callback
`CommitOptions.OnReplicaError`) but never fails the primary commit.
A corrupt or missing primary can be repaired from the replica with
`pg_hardstorage repair manifest <deployment> <backup-id>`.

### Verification

Read paths always go through `ParseAndVerify`, which fails closed on
signature mismatch. The dedicated `ParseAttestationless` is used by
exactly two code paths — `repair manifest` and `repair attestation` —
whose entire purpose is to inspect a manifest with a broken signature.

---

## 5. Coordination layer

The smallest deployments don't need any coordination service. The
abstraction is thin enough that the right backend is chosen by
topology rather than configuration:

| Topology               | Backend                       | Extra services |
| ---------------------- | ----------------------------- | -------------- |
| One host, one PG       | SQLite                        | none           |
| One host, many PGs     | SQLite                        | none           |
| 2–5 agents             | PostgreSQL advisory locks     | one tiny PG    |
| Kubernetes (any size)  | `coordination.k8s.io/Lease`   | none           |
| Large bare-metal fleet | etcd / Consul (opt-in)        | optional       |

For Patroni-managed clusters, we **reuse** Patroni's existing DCS
(etcd/Consul/Zookeeper) by writing under our own keyspace
`/pg_hardstorage/<deployment>/...`. No second DCS, no coordination
tax.

The interface is small: a lease primitive (`Acquire`, `Renew`,
`Release`), a key-value primitive (`Get`, `CompareAndSwap`,
`Watch`), and a serialisation primitive (the per-deployment backup
lock). Every backend implements all three.

---

## 6. Plugin tiers

### Tier 1 — in-tree Go interfaces

First-party plugins (storage, compression, encryption, renderer,
sink) live in `internal/plugin/<kind>/<name>/` and self-register via
`init()`. Statically linked. One signed binary is easier to audit,
FIPS-build, and ship.

```go
type StoragePlugin interface {
    Name() string
    Open(ctx context.Context, cfg StorageConfig) error
    Put(ctx context.Context, key string, r io.Reader, opts PutOptions) (PutResult, error)
    Get(ctx context.Context, key string) (io.ReadCloser, error)
    Stat(ctx context.Context, key string) (ObjectInfo, error)
    List(ctx context.Context, prefix string) iter.Seq2[ObjectInfo, error]
    Delete(ctx context.Context, key string) error
    RenameIfNotExists(ctx context.Context, src, dst string) error
    SetRetention(ctx context.Context, key string, until time.Time, mode WORMMode) error
    Capabilities() Capabilities
    Close() error
}
```

Sink, Renderer, CompressionPlugin, EncryptionPlugin follow the same
shape. New backends self-register; binaries that don't link a plugin
don't pay for it.

### Tier 2 — stdio JSON-RPC (shipped in v1.0)

Third-party plugins ship as separate binaries, discovered on
`$HSPLUGIN_PATH`.  Crash-isolated (a plugin crash doesn't take down
the agent), language-agnostic.  The runtime that loads them and
the `--probe` handshake ship in v1.0; see the
[build-a-storage-plugin tutorial](../tutorials/build-a-storage-plugin.md)
for a working example.  The proto-defined wire format (`.proto`
files in `internal/plugin/external/`) is the v1.x target; today's
contract is stdio JSON-RPC.

---

## 7. Output architecture

Every user-visible output — CLI command results, streaming progress,
audit events, alerts, errors — is a strongly-typed `Event` value
flowing through a unified pipeline. Two plugin tiers consume those
events:

- **Renderer** — synchronous, command-scoped: takes events, writes
  bytes to a `Writer` (stdout, stderr, file). Examples: `text`,
  `json`, `ndjson`, `yaml`, `template`.
- **Sink** — asynchronous, system-scoped: takes events, fans out to
  external systems on its own schedule. Examples: `slack`, `webhook`,
  `syslog`, `email`, `jira`, `opsgenie`, `pagerduty`.

The shape:

```go
type Event struct {
    Schema       string       // "pg_hardstorage.v1"
    Severity     Severity     // RFC 5424: emerg=0..debug=7
    SeverityName string       // "info", "warning", ...
    Component    string       // "backup", "wal.stream", "doctor", ...
    Op           string       // "backup_started", "progress", "wal_gap_detected"
    Subject      Subject      // {Tenant, Deployment, BackupID, Timeline, ...}
    Body         any          // typed payload (per Op)
    Suggestion   *Suggestion  // optional remediation directive
    GeneratedAt  time.Time
}
```

A single Dispatcher walks one Renderer (the active output mode) and N
Sinks in parallel. Sinks declare a severity floor and component
allow/deny filters; renderers don't filter (the active renderer
always renders what the user asked the command to do).

The same payload that the streaming gRPC RPCs produce is what shows
up in `-o ndjson` and what reaches the `slack` / `jira` sink. One
schema, one shape.

A panicking sink is recovered; siblings still receive the event; a
diagnostic line goes to stderr.

---

## 8. Resilience design

### At-rest checksums

Every chunk PUT carries the plaintext SHA-256 (as `x-amz-checksum-
sha256` on S3 backends, as a sidecar attribute on filesystem
backends). The backend verifies on receive; mismatch retries with a
fresh hash.

### Read-after-write verify

Every committed manifest is re-read once with `Get` and compared to
the canonical bytes that were written. Catches the rare "S3 said OK
but no" cases. One extra round-trip per backup commit; trivial cost.

### Scrub

`pg_hardstorage repair scrub <repo>` walks every chunk, decrypts (if
encrypted), re-hashes, and surfaces mismatches as
`verify.scrub_mismatch` (exit 9). Bit-rot at rest is detected and
reported.

### Repair toolkit

Explicit, never magic:

```
pg_hardstorage repair manifest    <deployment> <backup-id>
                                  # rebuild from replica copy
pg_hardstorage repair chunks      --orphans [--apply]
                                  # find chunks unreferenced by any manifest
pg_hardstorage repair chunks      --missing
                                  # find manifests that reference missing chunks
pg_hardstorage repair scrub
                                  # full SHA round-trip
pg_hardstorage repair slot        <deployment>
                                  # alias for `wal repair`
pg_hardstorage repair attestation <deployment> <backup-id>
                                  # re-sign a manifest after KEK rotation
pg_hardstorage repair index
                                  # walk chunk inventory, flag unparseable filenames
```

Each is dry-run by default; `--apply` performs writes. Each emits an
audit event.

---

## 9. Patroni failover handling

Patroni promotion increments the timeline ID. Any WAL written on the
old primary that hadn't replicated is on the old timeline, forked
from the new one. `pg_hardstorage` handles this through four
cooperating mechanisms:

1. **Leader-following.** Agent polls Patroni's REST API
   (`GET /cluster`, `GET /leader`) or watches the DCS. On leader
   change: stop the active replication connection cleanly, reconnect
   to the new leader, run `IDENTIFY_SYSTEM` to confirm
   `system_identifier` matches, run `TIMELINE_HISTORY <new_tli>` to
   capture the `.history` file into the repo.
2. **Slot continuity (three strategies, ranked; all shipped in v1.0).**
   - **Strategy A (recommended):** Patroni `permanent_slots` —
     Patroni recreates the slot on the new leader, slot's
     `restart_lsn` propagated by Patroni's slot-advance logic.
     Residual gap is at most one Patroni cycle of WAL.
   - **Strategy B:** PG 17+ synced slots. Native, no Patroni edit
     required.
   - **Strategy C (fallback):** recreate the slot on detection.
     The agent runs `IDENTIFY_SYSTEM` after reconnect; if the slot
     doesn't exist, recreate it and report any gap loudly.
3. **Dual-slot mode** (≥ 50 TB, opt-in via a `patroni.slots:` list
   of `{name, role}` entries).  Two slots on two nodes feed the
   same CAS; either stream can fail without RPO impact.
4. **Synchronous-target mode** (`wal_mode: synchronous`) — *not
   implemented*.  The aspiration is for the agent to advertise as a
   `synchronous_standby_names` candidate so PG waits for our flush
   ACK before commit (RPO = 0 at the cost of write latency).  Today
   there is no `wal_mode: synchronous` config key; only preflight
   *detection* of where `sync_standby` slots are placed ships.

Strategy C is honest about loss — it never silently glosses over a
gap. Manifests record the gap; PITR inside the gap window is
explicitly refused.

---

## 10. Repo on-disk layout

Same on disk and on object stores; just different prefix conventions.

```
<repo-root>/
  HSREPO                                              # magic: schema + repo id + tenants
  config/repo.json
  config/deployments/<deployment>.json
  chunks/sha256/aa/bb/aabb<rest>.chk                  # 2/2/60 split avoids wide listings
  manifests/<deployment>/backups/<id>/
      manifest.json
      manifest.idx                                    # binary sidecar (post-v1.0; tracked in SPEC_DRIFT)
      attestation.intoto.jsonl                        # cosign / in-toto (shipped)
      verification.json                               # appended by verifier
  manifests/_replicas/<id>.manifest.json              # redundant copy
  manifests/<deployment>/timeline/<tli>.json
  wal/<deployment>/timelines/<tli>.history            # captured on first sight
  wal/<deployment>/<timeline>/<prefix>/00000001000000000000000A.wal
  audit/<yyyy>/<mm>/<dd>/<seq>-<id>.json              # WORM bucket if available
  locks/<deployment>/<resource>.lock                  # CAS leases
  _trash/<deployment>/<id>.json                       # soft-deleted manifests
```

The chunk path layout — `chunks/sha256/aa/bb/aabb<rest>.chk` — is a
2/2/60 split. Object stores hate wide listings; this caps directory
fan-out at 256 even at very large scale. SHA-256 prefix collisions
are a non-issue at the cardinalities we ship.

The 24-month schema commitment applies to every on-disk file: HSREPO,
manifest, tombstone marker, hold marker, audit event, chunk envelope.
Readers accept the current schema version and the previous one
(e.g. envelope v0x01 legacy + v0x02 with encryption).
