# pg_hardstorage — PostgreSQL Backup, Done Right (Go)

> Plan status: ready for review (rev 3).
>
> **Confirmed decisions:** equal-weight Kubernetes + VM/bare-metal · serves single-org enterprises and multi-tenant SaaS equally · Apache 2.0 · **zero extra coordination services for ≤ 5-agent fleets** (no etcd needed) · **WAL streaming is the central data plane** · **logical decoding is a first-class second stream**.
>
> **Design north stars (in priority order):** Resilience · Compliance · Simplicity · Scale-spanning (10 GB ↔ 100+ TB).

---

## What it feels like (the UX manifesto)

This is the *whole* user-visible surface for the 90% case. Every line is a real command we will ship.

```
# Day 0 — five minutes after install. Interactive, but every prompt has a sensible default.
$ pg_hardstorage init
  ? Connect to PostgreSQL (postgres://...): postgres://backup@db1.example.com/postgres
  ? Where should we store backups? s3://acme-pg-backups/
  ? Encryption (recommended)? yes  (will generate a passphrase, write to ~/.pg_hardstorage/keyring)
  ? Retention? 7 daily / 4 weekly / 12 monthly  (default)
  ? Take a backup right now? yes
  ✓ Connected to PostgreSQL 17.2
  ✓ Repository ready (s3://acme-pg-backups/)
  ✓ WAL streaming started (replication slot pg_hardstorage_db1)
  ✓ Backup db1.full.20260427T0900Z complete · 12.3 GB physical · dedup ratio 1.4×
  ✓ Verified (pg_verifybackup OK)
  Next backup scheduled at 04:00 UTC daily. Next restore drill: Sunday.

# Day 1+ — daily life. Most users only ever need these.
$ pg_hardstorage backup db1                      # take a backup right now
$ pg_hardstorage status                          # one screen, every deployment
$ pg_hardstorage status db1                      # detail for one
$ pg_hardstorage list db1                        # list backups
$ pg_hardstorage logs db1                        # recent activity, tailable

# The 3am restore. "Not so skilled people need to work at night."
$ pg_hardstorage restore db1                     # interactive: pick a backup, confirm, go
$ pg_hardstorage restore db1 latest              # restore latest, with one confirmation
$ pg_hardstorage restore db1 --to "5 minutes ago"        # natural-language PITR
$ pg_hardstorage restore db1 --to "2026-04-27 09:42 UTC"
$ pg_hardstorage restore db1 --preview           # explain what would happen, RTO estimate, no changes

# Self-diagnosis. We expect users to run this when something feels off.
$ pg_hardstorage doctor                          # checks every deployment, prints what's wrong + how to fix it
$ pg_hardstorage doctor db1                      # one deployment

# Power-user when needed.
$ pg_hardstorage verify db1 latest               # restore to sandbox + pg_amcheck (proves restorability)
$ pg_hardstorage deployment add db2 --connection postgres://...   # add another DB
$ pg_hardstorage schedule db1 "every 6 hours"
$ pg_hardstorage rotate                          # apply retention now (also runs after each backup)
```

That is it. Subcommands beyond these exist (`gc`, `kms`, `agent`, `server`, `repo`) but a typical user never calls them — they're for automation, fleet ops, and break-glass.

**`pg_hardstorage status` (always-on dashboard) renders this in <0.5s for any deployment:**

```
db1  PG 17.2  primary @ db1.example.com:5432
  Last backup     47m ago  · full · 12.3 GB · ✓ verified
  WAL streaming   active   · lag 12s · slot pg_hardstorage_db1
  RPO / RTO       47m / ~4m (estimate)
  Repository      s3://acme-pg-backups/  · 142 GB used  · 12 backups retained
  Next backup     04:00 UTC (in 5h 13m)
  Next drill      Sunday 02:00 UTC
  Health          ✓ all clear
```

If health is not clear the line ends with the most actionable next step, e.g. `✗ KMS unreachable — run 'pg_hardstorage doctor db1 --suggest'`.

---

## Context

Greenfield Go project at `/Users/hs/projects/vibe/pg_hardstorage` (empty). The user wants a backup tool that is **resilient first, compliant second, easy to use third, and works from a 10 GB toy database to a 100+ TB production fleet without changing tools**. Not a pgBackRest clone. Cooler, better, nicer — and specifically built for the *3am tired operator* who is the realistic restore-time persona.

PG 15+. WAL transport prefers the **PostgreSQL replication protocol over a database connection** (works on managed services like RDS, doesn't need OS access on the host) — file-system archive paths are belt-and-suspenders, never primary.

---

## Design principles

1. **Resilience above all.**
   Every operation is idempotent. Every commit is atomic via content-addressed storage + CAS rename. No backup chain — every backup is independently restorable. Crash-only design: no graceful shutdown is required for correctness. Pre-flight checks block destructive operations. Plain-English errors with a "next step" suggestion baked into every failure.

2. **Compliance is a feature, not a tier.**
   Encryption, KMS, audit log, signed manifests are *on by default*. WORM, FIPS, crypto-shred ship as standard, not as paid add-ons (Apache 2.0 — there is no paid add-on).

3. **Simplicity is the headline product.**
   Common workflows are one command. Defaults are correct. We ship retention, verification, scheduling, alerting *built-in*, not bolted on. The 3am operator must succeed without reading docs.

4. **Scale-spanning.**
   Same binary, same UX, same config file from a 10 GB single-host PG to a 100+ TB Patroni cluster on Kubernetes. Big-database features (parallel chunk pipeline, snapshot base backups, multiple WAL streams from replicas) are *automatic upgrades* the system picks based on database size and topology — not a separate "enterprise mode."

5. **WAL via the replication protocol, not URLs.**
   Default WAL transport is `START_REPLICATION` over libpq. It works on managed PostgreSQL where you can't install an archive library. It survives network blips because of the persistent slot. The C archive-library extension is an *optional* secondary path for environments where pulling can't keep up.

6. **No magic strings, no jargon.**
   We say *deployment* and *backup* and *restore*. Not "stanza", not "repo node", not "diff backup". The command tree reads like English.

---

## Requested-features map

| Req | Where it lives |
| --- | --- |
| a) Multiple **storage** plugins | `internal/plugin/storage/{s3,fs,azure,gcs,sftp,tar}` + Tier-2 `go-plugin` for 3rd-party |
| b) Multiple **base-backup** plugins | `internal/pg/basebackup/` (streaming); snapshot plugin planned |
| c) **REST + CLI** (and gRPC) | `internal/api/{rest,grpc}`, cobra CLI in `internal/cli` |
| d) Storage-side **dedup** | Content-addressed chunks (FastCDC + page-aligned splits, SHA-256-keyed) |
| e) Optional **encryption + compliance** | Envelope encryption, pluggable KMS, cosign attestation, WORM, FIPS, audit |
| f) **Multi-operator** integration | Generic `HSDeployment` CRDs, CNPG-I provider, WAL-G CLI shim, pgBackRest CLI shim |
| g) WAL **archiving + streaming** | **Replication-protocol streaming is the data plane** (single-stream / replica-offload / dual-stream / sync-target / cascading auto-modes); `archive_library` + `archive_command` shims are optional belt-and-suspenders |
| g+) **Logical decoding stream** | Optional second stream per deployment via `START_REPLICATION ... LOGICAL`; output plugins for chunked CDC backup, Kafka/Pub/Sub fan-out, source-side PII redaction, cross-version restore |
| h) **Patroni** | `/leader`-aware agent, DCS-backed lease, `bootstrap.method: pg_hardstorage` |
| i) **Monitoring** | Prometheus + OpenTelemetry + structured JSON logs + audit log + `doctor` self-diagnosis |
| j) **Multi-instance** | Single agent multiplexes many deployments per host; control plane optional for single-host, mandatory for fleet |
| k) **Test backups** | Verifier subsystem: scheduled sandbox restore + `pg_verifybackup` + `pg_amcheck` + smoke SQL |
| l) **COW filesystems** | ZFS / Btrfs / LVM-thin / cloud-volume snapshots via `snapshot` source plugin |
| m) **PG 15+** | New `pg_backup_start/stop`, `archive_library` (PG 15+), `summarize_wal` + `pg_combinebackup` (PG 17+) |

**Bonus features (resilience, compliance, and what the gap-closures section adds):**

- **Resumable backups** at any byte offset (chunks already in repo are never re-uploaded).
- **Self-healing on startup** — agent reconciles `state/inflight.json` cleanly; never leaks `pg_backup_start`.
- **Built-in retention rotation** (GFS by default: 7 daily / 4 weekly / 12 monthly / 5 yearly + N days WAL).
- **Built-in scheduling** (no cron required) with declarative `every 6 hours` / cron syntax / one-shot.
- **Built-in coordination without etcd** — small JSON state files for single-host, PG advisory locks (in any reachable PostgreSQL) for small fleets, K8s Leases on K8s. etcd only for very large bare-metal fleets. We never ship an embedded SQLite to manage backups of a database the operator already runs.
- **Notifications by default** — `pg_hardstorage notify add slack ...` and you get alerts on backup failure, WAL gap, verification failure.
- **`doctor` command** — runs every health check and prints a remediation playbook for each problem.
- **`--preview` and `--dry-run` everywhere** that mutates state.
- **Restore preview** — explain what will happen, where data lands, RTO estimate, before any byte is restored.
- **Cosign attestations + Rekor transparency** on every backup.
- **Per-tenant KEK** with **GDPR crypto-shred API** baked in.
- **Anomaly detection** on backup size/duration/page-churn baselines.
- **Air-gapped bundle export** for offline transport.
- **PII redaction restore plugin** (integrates with `anon`) for non-prod restores.
- **Fleet-wide search** ("find a backup containing table X at LSN ≥ Y across all deployments").
- **Logical decoding stream** — per-table backup, source-side redaction, CDC fan-out, cross-version restore, time-travel queries.
- **Time-travel queries** — query any historical PG state without full restore (logical-stream-backed ephemeral instance).
- **Partial / table-level restore** — restore selected tables into a running database without touching the rest.
- **Hot-standby restore** — continuously-updating read-only replica fed entirely from the backup pipeline.
- **Synchronous backup target** — opt-in RPO=0 by acting as a `synchronous_standby_names` candidate.
- **Hash-chained Merkle audit log** with periodic transparency-log anchoring.
- **HSM / PKCS#11** for the most paranoid environments.
- **Legal hold**, **data residency pinning**, **data classification tags**.
- **n-of-m approvals** for destructive operations.
- **Restore runbook generator** for the 3am operator.
- **In-database SQL views** (`CREATE EXTENSION pg_hardstorage` → `pg_hardstorage.backups`, `pg_hardstorage.health`).
- **Machine-readable CLI output** — every command supports `--output json|ndjson|yaml|template` with a versioned `pg_hardstorage.v1` schema; auto-NDJSON when piped; structured errors with `suggestion.command` for scripting; stable exit codes (0–10). Designed-in v0.1.
- **FHS-clean filesystem layout** with `paths` resolution in code; lintian-clean Debian packaging + Fedora/RHEL spec; `pg-hardstorage-cluster` wrapper provides RHEL-style unified-view UX on Debian; systemd `pg_hardstorage@<deployment>.service` template for multi-instance.
- **Self-supervised agent** with cgroup self-limits and panic capture (supervisor package scaffolded, not yet implemented — systemd provides process supervision).
- **End-to-end checksums** on every storage write + read-after-write verification of manifests.
- **Periodic repository scrub** with auto-heal from replica region.
- **Restore checkpoints** + atomic target switch + multi-source chunk fetch + pre-flight throughput probe.
- **`pg_hardstorage repair`** subcommand suite for every form of corruption — designed, not yet implemented (`internal/repair/` is a scaffold; individual repairs handled inline).
- **Read-only repo mode** for incident response / forensics.
- **Automated game days** (failover simulation, S3 503 storms, agent kill -9 mid-backup) with reports attached to audit log.
- **Disaster runbooks** shipped with the binary, surfaced through `doctor`, customizable per deployment.
- **SLSA Level 3 build provenance** on every release artifact (planned).

---

## Vocabulary

| Term | Meaning |
| --- | --- |
| **Deployment** | A logical PostgreSQL service we back up. One Patroni cluster, one RDS instance, one CNPG `Cluster` — all called `deployment`. Replaces the word *stanza*. |
| **Backup** | One point-in-time recoverable artifact for a deployment. |
| **Restore** | The act of recreating a database from a backup (+ optional WAL replay for PITR). |
| **Repository** (or `repo`) | The destination where chunks, manifests, and WAL live (e.g. `s3://acme-pg-backups/`). One repo can hold many deployments. |
| **Tenant** | An isolation boundary. Maps to one customer in SaaS, or a logical zone like `prod`/`dev` in single-org. Each tenant has its own KEK. Single-org users get a default tenant they never see. |
| **Agent** | The long-lived `pg_hardstorage agent` process that does the work. Co-located with the DB host, or a remote agent talking to the DB over libpq. |
| **Control plane** | Optional in single-host mode; required for multi-host fleets. Schedules, RBAC, fleet view, verifier. |

---

## Coordination & dependencies (zero extra deps for the simple case)

We progressively layer coordination only when the topology demands it. **The small case has no extra services.**

| Topology | What runs | What we use to coordinate | Extra services needed |
| --- | --- | --- | --- |
| Single host, single PG | `pg_hardstorage` binary as systemd unit | Small JSON state files under `<state>/bookkeeping/`; no leader election needed | **none** |
| Single host, many PGs | One agent multiplexes them | Same JSON state files, per-deployment | **none** |
| 2–5 agents, on-prem or cloud VMs | Agents + control plane | **PostgreSQL advisory locks** (in a `pg_hardstorage` schema in any reachable PG) + **CYBERTEC pg_timetable** for declarative scheduling | **none new** if they have PG; pg_timetable is a thin add-on to that PG |
| Kubernetes (any size) | Sidecars or a Deployment | **Kubernetes `coordination.k8s.io/Lease`** objects for leader election | **none** — uses what K8s already gives |
| Large fleet (> 5 agents) on bare-metal | Agents + HA control plane | PG advisory locks **or** etcd/Consul (opt-in for very strict HA) | optional etcd/Consul |
| Multi-region active-active control plane | Agents + multiple control planes | etcd / Consul / Postgres logical replication | etcd or Consul |

**Concrete consequences:**
- The `pg_hardstorage init` wizard never asks about etcd unless the topology is "large fleet, on-prem, strict HA". The 90% case is one binary + one config file + one repo URL.
- Patroni-managed clusters: we **reuse** Patroni's existing DCS (etcd/Consul/Zookeeper) by writing under our own keyspace `/pg_hardstorage/<deployment>/...`. No second DCS, no coordination tax.
- Embedded mode: the same binary is agent + minimal control plane in one process, with bookkeeping in small operator-readable JSON files under `<state>/bookkeeping/`. Restarting the binary is the entire HA story (atomic `tmp+rename` writes give us crash safety).
- "Two agents in two AZs for redundancy" — the smallest non-trivial case — needs only **one shared PG** (often the same one being backed up has a `pg_hardstorage` schema, or any other PG anywhere). Advisory locks give us correctness without any additional service.
- **No embedded SQLite anywhere.** We never ship a non-PG database to manage PG backups. Persistent state goes into PostgreSQL (the operator already runs one) or etcd (when the topology is K8s-native or beyond a single-PG fit). pg_timetable from CYBERTEC is the recommended scheduler for fleets that want declarative SQL-driven schedules.

This is a deliberate simplification over pgBackRest (where multi-host needs careful repo-host setup) and over Barman (which traditionally needs a barman server VM).

---

## Resilience design

The non-negotiable behaviors:

1. **Idempotency** — Every action can be retried safely.
   - Chunks: content-addressed, written with `IfNotExists`. A retried upload is a no-op.
   - Manifests: committed via `RenameIfNotExists(.tmp → final)`. Either visible or invisible; never partially visible.
   - WAL segments: same rename-on-commit pattern.
2. **No backup chain dependency** — Even an "incremental" backup, after our chunker hashes its files, is reduced to a list of (existing or new) content-addressed chunks. A corrupt or deleted incremental does **not** invalidate any other backup. This is the main thing we do better than pgBackRest's incremental.
3. **Crash-only** — No clean-shutdown required. Agent on restart reads `state/inflight.json`:
   - If a backup was open: issue `pg_backup_stop(false)` to release server-side state, mark manifest as `aborted` (never committed).
   - If a chunk upload was in flight: just retry; CAS makes it safe.
   - If a WAL stream was active: reconnect to the slot at the saved LSN.
4. **Pre-flight checks** before every destructive op — `restore`, `delete`, `kms shred`, `repo gc`. Each prints a checklist:
   ```
   $ pg_hardstorage restore db1 --to "5 minutes ago"
     ✓ Repository reachable (s3://...)
     ✓ KMS key reachable (aws-kms://...)
     ✓ Backup chain selected: full 09:00 + WAL up to 09:42
     ✓ Target directory empty (/var/lib/postgresql/restored)
     ✓ Disk space available: 412 GB free, 280 GB needed
     ✓ Patroni: db1 not currently primary (we will not stomp on a live DB)
     This will restore PostgreSQL 17.2 data to /var/lib/postgresql/restored.
     RTO estimate: ~4 minutes.
     Continue? [y/N]
   ```
5. **No hidden state.** The repo is the source of truth. Local agent state is regenerable (caches: bloom filter of chunks, manifest index). If you `rm -rf /var/lib/pg_hardstorage/cache`, the agent rebuilds it.
6. **Manifest redundancy.** Critical metadata (manifest + index sidecar + signature) is written **twice**: once at `manifests/<deployment>/backups/<id>/manifest.json`, once at `manifests/_replicas/<id>.manifest.json` in the same repo. Cheap, prevents single-key corruption disaster.
7. **Plain-English errors with remediation.** Every error type carries a `Suggestion` field. The CLI prints it; the API exposes it as a JSON field; structured logs include it.
   ```
   ERROR: WAL stream replication slot 'pg_hardstorage_db1' is not present on the server.
   What to do: the slot was probably dropped by an admin. Recreate it with:
     pg_hardstorage wal repair db1
   This will create a new slot and bootstrap from the latest backup's stop_lsn.
   ```
8. **Backups of backups (cross-region).** A single config flag `repo.replicate_to: ['s3://acme-pg-backups-eu/']` enables async cross-region copy of every committed manifest + its chunks. Implemented as a goroutine in the agent that watches the manifest stream.
9. **Dead-man's switch.** If no successful backup happens in `N×scheduled_interval` for a deployment, the control plane raises a `backup_overdue` alert through every configured notifier. Same for WAL: if no segment archived in `M minutes`, raise `wal_silence`.
10. **Confirmation gates** on destructive operations with `--yes` for automation. The default UX is "we will not let you shoot your foot off".

---

## Resilience engineering — concrete safeguards

The 10 principles above are the *what*. This section is the *how* — the specific engineering we put in to make crashes, hangs, partial failures, partition events, slow networks, and corrupted state recoverable rather than catastrophic.

### Process supervision & memory safety

- **Self-supervised agent.** [Planned, not yet implemented — package `internal/supervisor/` exists as a scaffold.] The agent will run as a parent that fork-execs a worker child. The parent is tiny (< 5 MB RSS), only watches the child via a Unix-socket heartbeat (1 Hz) and `waitpid`. If the child dies or stops sending heartbeats for 30 s, the parent rotates logs, captures a crash bundle, and re-execs the worker. Works under systemd, in containers, on raw VMs. systemd's `Restart=always` is layered on top for double-coverage.
- **cgroup self-limits (Linux).** Agent writes its own cgroup at startup with `memory.max` (default 70% of host) and `cpu.max` configurable. Approaching limits triggers the chunker pipeline to back off rather than the kernel OOM-killing us mid-`pg_backup_stop`.
- **mmap'd inflight state.** Large in-flight buffers (per-segment WAL, in-progress chunk batches) are mmap'd files, not heap allocations. If we OOM-kill, the kernel still flushes dirty pages. On restart, the worker reads the mmap, reconciles with the repo (CAS makes this safe), and continues.
- **Panic capture.** Every goroutine top-level wraps `defer recover()` → write a JSON crash report to `/var/lib/pg_hardstorage/crashes/<ts>.json` (state snapshot, last operation, stack trace, env) and re-panic so the supervisor restarts. Reports are auto-uploaded as audit events for post-incident review.
- **Sanitizer-built C extension.** The `pg_hardstorage_archive` extension is < 200 LOC but it links into the postmaster — that's a real-world concern. CI builds it with `-fsanitize=address,undefined` and runs the integration suite under sanitizers; production binaries strip the sanitizers but inherit the bug-finding work.
- **Memory accounting at every layer.** Chunker, encryption, storage upload — each carries a `MemBudget` token; if the budget is full, the goroutine blocks. No unbounded queues anywhere (no silent OOM waiting to happen).

### Storage-layer hardening

- **End-to-end checksums on every write.** Chunk PUTs carry the SHA-256 of the plaintext as `x-amz-checksum-sha256` (or backend equivalent). Backend validates on receive. Mismatch → retry with fresh hash.
- **Read-after-write verification.** Every committed manifest is re-read once with `Get` and the canonical bytes compared. Catches the rare "S3 said OK but no" cases. Costs one round-trip per backup commit; trivial.
- **Periodic scrub job.** A `repo-scrub` worker walks N% of chunks per day and re-hashes them. Bit-rot or backend corruption is caught and reported. If a replica region is configured, scrub auto-heals from the replica.
- **Replica is independently restorable.** Cross-region copy isn't just a tarball — the replica region has its own manifest copies, its own attestation, its own KMS reference. A primary-region wipe is survivable.
- **Conditional writes only.** All chunk uploads use `If-None-Match: *` (or backend equivalent) so a retry never overwrites a successful upload. Manifests use `RenameIfNotExists` for the same reason.
- **Repo capacity check before commit.** Pre-flight asserts the repo has at least 110% of the projected backup size free. Refuses to start an in-flight backup that would fail mid-flight from a full bucket / disk.

### Network resilience

- **Exponential backoff with full jitter** (AWS pattern), separate retry budgets for transient (`429`, `503`, network errors) vs permanent (`403`, `404`) errors, circuit breakers per backend host. A flaky region throttles its own queue rather than starving everyone else.
- **Connection reuse.** HTTP/2 multiplexed connections to S3 / Azure / GCS, kept warm with periodic `HEAD /`. Avoids TCP slow-start and TLS handshake on every chunk upload. ~30% throughput improvement on small chunks.
- **Happy Eyeballs (RFC 8305).** IPv4 + IPv6 attempted in parallel; faster path wins. Avoids minute-long IPv6 resolution stalls.
- **Bandwidth budgets enforced at the chunker.** If the storage backend can sustain 200 MB/s and the chunker can produce 800 MB/s, the chunker blocks rather than buffer-bombing memory. Backpressure is *the* primary mechanism for resource safety.
- **Adaptive concurrency.** Concurrent uploads start at 4 and ramp up based on observed RTT and error rate (TCP-style); ramp down on errors. No hand-tuned magic numbers.

### PostgreSQL-level safety

- **Slot keepalives.** Agent sends `Standby Status Update` messages at 5 s intervals (well under PG's `wal_sender_timeout` default of 60 s). A network blip up to ~10 s tolerated transparently.
- **Long-running `pg_backup_start` watchdog.** A `pg_backup_start` should normally release within minutes. If our deferred `pg_backup_stop` hasn't fired within `2 × expected_backup_duration` we log a critical alert; if a clean abort fails we escalate. We never want to be the long-running transaction holding xmin and bloating the cluster.
- **Checkpoint pacing.** We use `pg_backup_start(label, fast=false)` by default — letting PG checkpoint at its own pace — to avoid I/O storms on busy systems. `fast=true` is opt-in for "I want the backup to start *right now*."
- **Backup-side I/O throttling.** Configurable `max_io_mb_per_second` per deployment so a 100 TB backup doesn't starve user queries. Default: unbounded; production tuning recommended.
- **Replica preference at scale.** Auto-routes backups to a Patroni replica at ≥ 5 TB to keep primary I/O free.

### Restore-time resilience

- **Restore checkpoints.** During a long restore, the in-progress data dir is fsynced every 1 GB extracted; a manifest of "what's been written so far" lives at `<target>/.pg_hardstorage_restore_state.json`. A crash mid-restore resumes from the last checkpoint, not from scratch.
- **Atomic target switch.** Restore writes to `<target>.staging`, then `rename(<target>.staging, <target>)` once verified. A crash leaves `<target>` either untouched (good) or fully populated (good). Never half-populated.
- **Refusal to restore over a live PG.** Agent checks `<target>/postmaster.pid` and refuses unless `--force` (with a confirmation prompt that includes the PID it's about to overwrite).
- **Multi-source restore.** Chunks are fetched from primary and replica regions in parallel; whichever responds first wins. A degraded primary region doesn't block a restore that the replica can serve.
- **Pre-flight throughput probe.** Before kicking off a multi-hour restore, agent runs a 30-second probe (sequential reads from repo, sequential writes to target) and prints projected RTO. If projected RTO blows the SLO, surface a warning before the user commits to the operation.
- **Verified before declaring success.** Restores are not "successful" until `pg_verifybackup` passes. The CLI exit code reflects this. Operators can `--skip-verify` only with an explicit acknowledgement flag that is captured in the audit log.

### Repair toolkit (explicit, never magic) — planned, not yet implemented

The repair toolkit is designed but the `internal/repair/` package is a scaffold
with no implementation yet. The subcommand surface below is the target; `doctor`
and individual repair paths (e.g. `wal repair`, `slot repair`) are currently
handled inline within their respective packages.

```
pg_hardstorage repair manifest    <deployment> <backup-id>
                                  # rebuilds from replica copy or chunk-reference index
...
  1/<prefix>/...wal    # WAL files per timeline (already in the design)
  2/<prefix>/...wal
  3/<prefix>/...wal
```

Manifests carry the timeline they ended on:

```json
{ "timeline": 2, "stop_lsn": "0/30001A0", "wal_required": ["..."] }
```

Restore (PITR) walks the timeline history to reconstruct the chain: target LSN on TLI 3 → switch point on TLI 3 → resume on TLI 2 → switch point on TLI 2 → resume on TLI 1 → base backup on TLI 1. PG's recovery understands timeline history natively; we just need to ensure all `.history` files are in the repo at restore time. We always fetch them on every connection, so they're always there.

### What `doctor` reports for a Patroni cluster

```
$ pg_hardstorage doctor db1
  db1 — PG 17.2 — Patroni 3.3.1 — leader: node-2 (since 2026-04-28 09:12)
    ✓ Patroni REST reachable (3 nodes, all healthy)
    ✓ Slot continuity strategy: A (Patroni permanent_slots)
    ✓ Slot 'pg_hardstorage_db1' present on all 3 nodes
    ✓ WAL streaming active from leader node-2, lag 8s
    ✓ Last 3 timelines captured: TLI 1, 2 (switched at 0/15A2B388), 3 (switched at 0/2400FF80)
    ✓ Last failover: 47h ago, gap during failover: 0 bytes (dual-slot)
    ✓ Backup posture: tier-1 (sync-target NOT enabled, RPO target met by streaming + dual-slot)
```

### Failover-mode interactions and the explicit refusals

- `pg_rewind` removes WAL from the rewound node. We never back up from a non-leader; Patroni REST is the source of truth.
- A split-brain scenario where two nodes claim leader: agent refuses to back up either, emits a critical alert. The DCS-backed lease guarantees only one agent commits a manifest; both connections may be open but at most one progresses past `pg_backup_stop`.
- Bootstrap of a brand-new replica via `bootstrap.method: pg_hardstorage`: restore runs against the repo, not against the live cluster, so it's failover-immune by construction.

### What this means for the v0.1 cut

v0.1 ships: leader-follow via Patroni REST, Strategy A (Patroni `permanent_slots` integration via `init --patroni`), Strategy C slot recreation with explicit gap detection, full timeline-history capture and storage. Dual-slot and sync-target modes land later.

---

## Logical decoding (the second stream — first-class)

Physical WAL is the truth-of-record (full database recovery, byte-identical restore). **Logical decoding is an *additional* stream**, configured per deployment, that decodes WAL into row-level changes. It does not replace physical WAL — it complements it.

### What logical decoding unlocks

| Feature | Mechanism | Why it matters |
| --- | --- | --- |
| **Per-table / per-schema backup policies** | Logical slot with publication filter | "Back up `prod.users` separately from `prod.events`, with stricter retention and a different KEK." This is impossible with physical WAL. |
| **Source-side PII redaction** | Custom output plugin scrubs columns before chunks land | Compliance: PII never touches the backup repo. Crypto-shred is no longer the only mitigation. |
| **CDC fan-out** | Same logical stream tees to a `Notifier` plugin (Kafka, Pub/Sub, webhook, S3 event stream) | One pipeline, two outputs. Backup + analytics dataflow share infrastructure. |
| **Cross-major-version restore** | Logical backups are PG-version-agnostic | "Restore my PG 15 backup into a PG 18 instance to test the upgrade." Physical backups can't do this. |
| **Sub-second RPO** | Continuous logical stream with minimal lag | For tier-0 systems where 12 seconds of physical WAL lag is too much. |
| **Time-travel queries** | Logical replay into a small ephemeral PG, query historical state | "What did `users.email` look like for user 42 on 2026-04-15?" — answer without doing a full restore. |
| **Hot standby off backups** | Continuously apply logical changes to a side replica | A read-only analytical replica fed entirely from the backup pipeline. |

### How it plugs in

```
PG primary
  ├── physical slot (mandatory) ────────► chunked physical WAL store (truth-of-record)
  └── logical slot(s) (optional) ─► output plugin ─► one or more sinks:
        e.g. pgoutput / wal2json / pg_hardstorage_proto
                                        ├── chunked logical stream (CDC backup)
                                        ├── Kafka / Google Pub/Sub
                                        ├── webhook / S3 event stream
                                        ├── PII-redactor → chunked redacted stream
                                        └── time-travel buffer (last 30 days, ephemeral)
```

We ship our own output plugin **`pg_hardstorage_proto`** (protobuf-encoded change events: small, schema-evolution-aware, fast). We also support stock `pgoutput` (built-in) and `wal2json` (popular, human-readable).

### Configuration

```yaml
deployments:
  db1:
    connection: postgres://backup@db1.example.com/postgres
    physical_wal: { mode: stream }                  # the data plane

    logical:                                        # opt-in, multiple allowed
      - name: cdc-events
        publication: "FOR TABLE public.events"
        output: pg_hardstorage_proto
        sinks:
          - chunked: { policy: 14d_retention }
          - kafka:   { topic: pg.events, brokers: ... }
      - name: pii-redacted
        publication: "FOR ALL TABLES EXCEPT public.users_pii"
        output: pgoutput
        transform: redact_email_phone
        sinks:
          - chunked: { policy: 7y_retention, kms_key: prod-redacted }
```

### Caveats we are explicit about

- Logical decoding does **not** capture DDL by default (PG 18 is improving this; we surface what's available per major version).
- The slot must be on the primary — replicas can host physical slots but logical slots are primary-only until logical-on-replica becomes mainstream.
- Logical replication imposes more CPU on the primary than physical. We expose `wal_sender_timeout`, `logical_decoding_work_mem` as tunables and surface them in `doctor`.
- Logical does not replace base backups — initial seed is still a physical full or a `COPY` snapshot.
- A logical-only deployment is **not** a full backup posture. We refuse to mark a deployment as "backed up" if only logical is configured.

---

## Backup workflow

Three modes, all producing manifests of identical schema. The user does not pick the mode; the agent does, based on the deployment profile.

**Streaming base backup (default for < 50 TB).**

1. Lease `locks/<deployment>/backup.lock` via CAS (TTL).
2. Replication-protocol connection to PG (or the Patroni-elected replica if size > 5 TB).
3. `SELECT pg_backup_start('pg_hardstorage_<id>', false);` (non-exclusive — only mode in PG 15+).
4. `BASE_BACKUP LABEL '...' MANIFEST yes PROGRESS` → tar streams per tablespace.
5. Streaming chunker pipeline: hash → bloom-filter check → `Stat` if missed → `Put(IfNotExists=true)` if absent. Encrypt-per-chunk with derived key.
6. `SELECT pg_backup_stop(true);` → `(lsn, labelfile, spcmapfile)`. **`labelfile` and `spcmapfile` MUST be persisted into the manifest** as `backup_label` and `tablespace_map`.
7. Wait for `pg_walfile_name(stop_lsn)` to exist in our WAL store (it usually already does, because streaming).
8. Compute manifest, cosign-sign, write `manifest.json.tmp`, `RenameIfNotExists` to commit. Drop lock.
9. **Auto-rotate**: apply retention policy. **Auto-verify**: schedule a verification job.

**PG 17 incremental.** Same flow with `BASE_BACKUP INCREMENTAL <prior-manifest>`. PG sends `INCREMENTAL.<file>` deltas; chunk-CAS catches everything else for free.

**Snapshot (default for ≥ 50 TB or whenever a COW FS / cloud volume is detected).**

1. `pg_backup_start('pg_hardstorage_snap_...', true);` (fast=true → CHECKPOINT).
2. Snapshot via `cow_driver`: `zfs snapshot tank/pgdata@hs-...` / `btrfs subvolume snapshot -r ...` / `lvcreate -s ...` / cloud-volume snapshot API (EBS, GCE PD, Azure).
3. `pg_backup_stop(true);` → write label + spcmap into the snapshot mount.
4. Mount snapshot read-only, walk it through the same chunker pipeline.
5. Optional `zfs send -i prev@hs @hs-current` for cross-host shipping (also captured into chunks).
6. Drop snapshot per retention.

**Failure handling:**
- `pg_backup_stop` MUST run. Deferred handler + `state/inflight.json` reconciler. On crash, agent issues `pg_backup_stop(false)` to release server lock without waiting for archive.
- Chunk-upload failure → retry (idempotent). GC reaps orphans.
- Manifest commit fails halfway → `.tmp` exists, no `.json`, never visible. GC sweeps stale tmps.
- Patroni leader change mid-backup → `/leader` watcher aborts; manifest never committed.

---

## Retention & rotation (built-in, automatic)

Default policy is **GFS** (grandfather-father-son), evaluated after every backup commit:

```yaml
retention:
  policy: gfs            # default
  keep_daily: 7
  keep_weekly: 4
  keep_monthly: 12
  keep_yearly: 5
  keep_wal_days: 14      # WAL retained for PITR window
```

Alternatives users can pick with one knob:

```yaml
retention:
  policy: simple
  keep_for: 30d          # everything younger than 30d is kept; older deleted
```

```yaml
retention:
  policy: count
  keep_full_count: 14    # keep last 14 fulls; WAL kept while needed for PITR back to oldest kept full
```

```yaml
retention:
  policy: regulatory
  keep_yearly: 7         # 7-year retention for compliance
  worm: true             # plus apply WORM lock (S3 Object Lock Compliance mode)
```

Rotation is a normal repo operation — it runs after each backup commit and as a separate scheduled job. Soft-delete first (manifest moved to `manifests/_trash/<id>.json` with TTL), GC sweeps chunks no longer referenced after the soft-delete grace period.

---

## Restore workflow (3am-friendly)

**Discoverable.** `pg_hardstorage restore db1` with no arguments enters interactive mode: list of recent backups with timestamps, sizes, and verification status. Pick one, confirm. Done.

**Natural-language time.** `--to "5 minutes ago"`, `--to "yesterday 9pm"`, `--to "2026-04-27 09:42 UTC"`, `--to-lsn 0/3000028`, `--to-backup <id>`. Parsed via a clear, predictable syntax (we vendor `tj/go-naturaldate`).

**Preview before action.** Every restore offers `--preview`:

```
$ pg_hardstorage restore db1 --to "5 minutes ago" --preview
  Would restore to:    /var/lib/postgresql/restored
  PostgreSQL version:  17.2
  Source backup:       db1.full.20260427T0900Z (full, 12.3 GB physical)
  WAL replay range:    0/3000028 → 0/30001A0  (~14 segments)
  Estimated RTO:       4 minutes
  Estimated disk:      280 GB
  Tablespaces:         pg_default → /var/lib/postgresql/restored (default)
  Verification:        ✓ pg_verifybackup will run after restore
  Run with --confirm to execute.
```

**Steps:**

1. Resolve target backup or chain (PITR → walk timeline files, pick latest backup with `stop_lsn ≤ target_lsn` + WAL).
2. Verify cosign signature on `manifest.json`; optional Rekor lookup.
3. If incremental chain: stage extracts, run `pg_combinebackup` to flatten.
4. Parallel chunk fetch + decrypt + verify-vs-declared-SHA-256 + write to output.
5. Apply `tablespace_map` (with `--tablespace-mapping=old=new` overrides).
6. Write `recovery.signal` / `standby.signal`; render GUCs into `postgresql.auto.conf`:
   `restore_command = 'pg_hardstorage wal fetch <deployment> %f %p'`, `recovery_target_*`.
7. **Mandatory gate**: `pg_verifybackup` against the data dir (`--skip-verify` warns).
8. Optional: start the cluster, wait for `recovery_target` reached, run `pg_amcheck --all`.
9. Emit `restore.completed` audit event with verification report.

**Refusal cases (the system says "no" with a plain reason):**
- Refuses to overwrite a non-empty target without `--force`.
- Refuses to restore over a live PG data dir.
- Refuses to restore on the current Patroni primary (would stomp the cluster) — suggests using a different node.
- Refuses if KMS key is unreachable (cannot decrypt).

---

## Enterprise features — gap closures (rev 3)

Audit of the original design surfaced these missing pieces. Many are now implemented; the table reflects current status.

### Identity, access, governance
| Feature | Status | Note |
| --- | --- | --- |
| **SAML 2.0 SSO** | Planned | Enterprise SSO is still SAML at many shops. OIDC + SAML side-by-side. |
| **LDAP / Active Directory** for group → role mapping | Planned | Groups drive RBAC; tenant scoping respected. |
| **SCIM 2.0** for user/group provisioning | Implemented | `internal/scim/` — auto-provision and de-provision human users. |
| **n-of-m approval workflow** for destructive ops | Implemented | `internal/approval/` — configurable threshold per op (`backup:delete`, `kms:shred`, `repo:gc`). |
| **Insider-threat anomaly detection** | Implemented | `internal/insider/` — unusual download patterns, novel IAM principals, off-hours bulk reads → alert. |
| **Just-in-time (JIT) access** | Implemented | `internal/jit/` — time-bound elevated tokens for break-glass restore; auto-expire; audit-stamped. |

### Cryptography & key custody
| Feature | Status | Note |
| --- | --- | --- |
| **PKCS#11 / HSM** support | Implemented | `internal/plugin/kms/pkcs11/` — nCipher, Thales, AWS CloudHSM, YubiHSM. |
| **Threshold signing (k-of-n)** for backup attestations | Implemented | `internal/threshold/` — multi-party signing for highest-assurance manifests. |
| **Hash-chained Merkle audit log** | Implemented | `internal/audit/`, `internal/chain/` — each audit event includes the prior event's hash → tamper-evident. Periodic anchor commits to Rekor / a customer-managed transparency log planned. |
| **Customer-managed key (CMK) BYOK** with attested rotation | Implemented | Already implicit in KMS plugin; explicit BYOK story documented. |

### Data lifecycle & legal
| Feature | Status | Note |
| --- | --- | --- |
| **Legal hold** | Implemented | `internal/hold/` — suspends deletion regardless of retention; clearable only by RBAC-authorized actor; recorded in audit. |
| **Data residency / sovereignty pinning** | Implemented | `internal/classify/` — per-deployment policy: backups must remain in `region in {EU}`; the storage plugin enforces. |
| **Data classification tags** | Implemented | `internal/classify/` — Public / Internal / Confidential / Restricted; drives retention floor, encryption requirement, allowed regions. |
| **GDPR data-subject-access (DSA) helper** | Implemented | `internal/dsa/` — given a subject ID, locates which backups contain their data; pairs with crypto-shred for erasure. |
| **Cross-account / cross-org repo replication** | Planned | M&A, partner-data scenarios. Async copy with explicit ACL boundary. |

### Operational
| Feature | Status | Note |
| --- | --- | --- |
| **RPO/RTO SLOs as code** | Implemented | `internal/slo/` — declarative per-deployment SLO; alert when missed; dashboard panel. |
| **Capacity planning report** | Implemented | `internal/capacity/`, `internal/forecast/` — 30/90/365-day projection of repo size, chunk-count, WAL volume; per-deployment. |
| **Cost reporting** | Implemented | `internal/cost/` — per-deployment repo cost (S3 + KMS + egress); billable export. |
| **Compliance report generator** | Implemented | `internal/compliance/` — auto-generated report mapping to SOC 2 / ISO 27001 / HIPAA / PCI / FedRAMP control IDs. |
| **Automated DR game day** | Implemented | `internal/gameday/` — opt-in scheduled chaos events; reports RTO actual vs SLO. |
| **Egress shaping per repo per time-of-day** | Planned | Bandwidth caps to avoid blowing through cloud-egress budget at month-end. |
| **Backup integrity continuous attestation** | Implemented | `internal/integrity/` — periodic re-hash of old chunks + manifest signature re-verify; finds bit-rot before restore. |
| **Status page / customer notifications** | Planned | Per-tenant subscription status page. |

### Restore tooling (the operator's actual job)
| Feature | Status | Note |
| --- | --- | --- |
| **Time-travel queries** | Implemented | `internal/timetravel/` — spin up an ephemeral read-only PG from any backup + WAL position; query historical state without full restore. |
| **Partial / table-level restore** | Implemented | `internal/partial/` — restore one or more tables (or schemas) into the running database. |
| **Hot-standby restore** | Implemented | `internal/standby/` — continuously-updating read-only replica fed entirely from the backup pipeline. |
| **Restore runbook generator** | Implemented | `internal/runbook/` — given a deployment + scenario, emit a step-by-step Markdown runbook with copy-pasteable commands. |
| **Multi-language CLI / TUI (i18n)** | Implemented | `internal/i18n/` — German, French, Japanese. |

### PostgreSQL-aware integrations
| Feature | Status | Note |
| --- | --- | --- |
| **TDE awareness (`pg_tde`, EDB TDE)** | Planned | Detect TDE state, preserve it through backup/restore, refuse to "encrypt twice" silently. |
| **`pgaudit` integration** | Planned | Stamp backup-related role activity into pgaudit; correlate with our audit log. |
| **In-database SQL views** | Implemented | `internal/dbext/` — `CREATE EXTENSION pg_hardstorage` exposes `pg_hardstorage.backups`, `pg_hardstorage.health`, `pg_hardstorage.rpo`. |
| **Logical decoding option** | Implemented | Full logical decoding stream with multiple sinks. See "Logical decoding" section. |

---

## Encryption & compliance

**Three-layer envelope:**

1. **Repository KEK (RKEK)** — held in configured KMS (AWS KMS, GCP KMS, Azure Key Vault, Vault Transit, or local AES-256-GCM with passphrase for dev). Reference stored in `HSREPO`.
2. **Backup DEK (BDEK)** — 256-bit random per backup, wrapped by RKEK. Stored in `manifest.json.encryption.wrapped_dek`.
3. **Per-chunk key**: `Kc = HKDF-SHA256(BDEK, info=chunk_hash)`. Cipher: **AES-256-GCM-SIV** (RFC 8452, nonce-misuse resistant) by default; AES-256-GCM with random 96-bit nonce in FIPS mode (BoringCrypto doesn't yet ship GCM-SIV).

**Per-tenant KEK** is mandatory architecture (single-org users get a default tenant). This makes **GDPR crypto-shred** a one-line operation:
```
pg_hardstorage kms shred --tenant T --reason "GDPR Art. 17 request #4421"
```
Schedules KMS deletion of T's KEK; backups stay bit-for-bit but become unrecoverable. Audit log entry with attestation is the compliance artifact.

**KEK rotation.** `pg_hardstorage kms rotate` walks all manifests, decrypts wrapped_DEK with old KEK, rewraps with new KEK, atomically rewrites manifest. Chunks are not re-encrypted. Old KEK retired after grace.

**Cosign attestations.** Every commit signs `manifest.json`. Optional Rekor transparency entry. Pubkey pinned in repo config. `pg_hardstorage backup show <id>` displays the attestation chain.

**WORM.** S3 Object Lock (Compliance mode), Azure immutable blob, NetApp SnapLock, generic POSIX appendix (chattr +i). `SetRetention` on the StoragePlugin propagates retention dates to the backend. Configured per deployment or per repository: `worm: true, retention: 7y`.

**Audit log.** Structured JSON, append-only, shipped to a separate WORM bucket. Every backup / restore / verify / KMS op has actor, deployment, backup_id, KEK ref, IP, RBAC scope. Queryable: `pg_hardstorage audit search --deployment db1 --since 30d --action restore`.

**FIPS.** Build `pg-hardstorage-fips` with `GOEXPERIMENT=boringcrypto`. Refuse to start if `crypto/tls` reports non-FIPS. `--fips-strict` panics on any non-FIPS plugin.

---

## Patroni & operator integration

**Patroni**, designed-in from v0.1:
- Per-deployment YAML `patroni:` block (`url`, `slot`, `interval`, `user`, `password`).  Pre-backup: `GET /leader` (200 = leader).  Configurable to prefer `GET /replica?lag=10000` for offload (default automatic at ≥ 5 TB).  (No top-level `--patroni-url` CLI flag — Patroni is per-deployment, and the agent processes many deployments per host.)
- Bootstrap: `bootstrap.method: pg_hardstorage` →
  `bootstrap.pg_hardstorage.command: '/usr/bin/pg_hardstorage restore ${SCOPE} --target ${PGDATA}'`.
- DCS coordination: backup-leader lease via etcd CAS on `/pg_hardstorage/<deployment>/backup-leader`.
- pg_rewind compatible (permanent slots).

**Kubernetes operator integration** — contracts designed-in, bridge code implemented:
- **WAL-G CLI shim** (`pg_hardstorage agent --walg-shim`), **pgBackRest CLI shim** (Crunchy PGO), **Barman CLI shim**, **Barman Cloud shims** — all via `pg-hardstorage-compat` multi-call binary.
- **Helm charts**: `charts/pg-hardstorage-server` (control plane) + `charts/pg-hardstorage-sidecar` (per-Pod sidecar with config injection).
- Generic CRDs (`pghardstorage.org/v1`: `HSDeployment`, `HSBackup`, `HSRestore`, `HSSchedule`) and CNPG-I provider are planned.

---

## Plugin model

**Tier 1 — in-tree Go interfaces** for first-party plugins (S3, FS, GCS, Azure Blob, SFTP, SCP — all KMS providers, all compressors, all renderers, all sinks). Statically linked. One signed binary is easier to audit, FIPS-build, ship. Backup source (streaming BASE_BACKUP) is in `internal/pg/basebackup/`; no separate SourcePlugin tier yet.

**Tier 2 — `hashicorp/go-plugin` (gRPC over Unix-domain stdio)** for *third-party* plugins. Author ships a separate binary; `pg_hardstorage` discovers it on `$HSPLUGIN_PATH`. Crash-isolated, language-agnostic. Public registry at `registry.pghardstorage.org` post-v1.0.

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

type SourcePlugin interface {
    Name() string
    Capabilities() SourceCapabilities                                  // {Full, Incremental, Snapshot, Streaming}
    Prepare(ctx context.Context, target PGTarget) (SourceSession, error)
}

type EncryptionPlugin interface {
    Name() string
    GenerateDEK(ctx context.Context, tenant TenantID) (dek []byte, wrapped []byte, err error)
    UnwrapDEK(ctx context.Context, tenant TenantID, wrapped []byte) ([]byte, error)
    RotateKEK(ctx context.Context, tenant TenantID, oldRef, newRef KeyRef) error
    Shred(ctx context.Context, tenant TenantID) error
    FIPSMode() bool
}
```

`CompressionPlugin`, `Renderer`, and `Sink` (replaces `Notifier`) follow the same shape — see "Output architecture" below for `Renderer` / `Sink` interfaces.

---

## Repository layout

Same on disk and on object stores; just different prefix conventions.

```
<repo-root>/
  HSREPO                                           # magic: {"version":1,"id":"...","tenants":[...]}
  config/repo.json
  config/deployments/<deployment>.json
  chunks/sha256/aa/bb/aabb<rest>.chk               # 2/2/60 split avoids wide listings
  manifests/<deployment>/backups/<id>/
      manifest.json
      manifest.idx                                 # binary sidecar: BLAKE3 path → offset
      attestation.intoto.jsonl                     # cosign / in-toto
      verification.json                            # appended by verifier
  manifests/_replicas/<id>.manifest.json           # redundant copy (resilience principle 6)
  manifests/<deployment>/timeline/<tli>.json
  wal/<deployment>/<timeline>/<prefix>/00000001000000000000000A.wal
  audit/<yyyy>/<mm>/<dd>/<event-id>.json           # WORM bucket if available
  index/fleet.bleve/                               # optional fleet-wide search (control plane)
  locks/<deployment>/<resource>.lock               # CAS leases
  _trash/<deployment>/<id>.json                    # soft-deleted manifests (TTL before GC)
```

**Manifest** (JSON, canonical):

```json
{
  "manifest_version": 1,
  "backup_id": "db1.full.20260427T093017Z",
  "deployment": "db1",
  "tenant": "default",
  "type": "full",
  "parent_backup_id": null,
  "pg_version": 170,
  "system_identifier": "7388123...",
  "start_lsn": "0/3000028", "stop_lsn": "0/30001A0",
  "timeline": 1,
  "compression": "zstd:9",
  "encryption": {
    "scheme": "aes-256-gcm-siv",
    "wrapped_dek": "base64...",
    "kek_ref": "aws-kms://arn:aws:kms:...:key/...",
    "envelope_version": 1
  },
  "tablespaces": [{"oid":1663,"location":"pg_default"}],
  "files": [
    {"path": "base/16384/2619", "size": 8192,
     "chunks": [{"hash":"aabb...","offset":0,"len":4096},
                {"hash":"eeff...","offset":4096,"len":4096}]}
  ],
  "wal_required": ["000000010000000000000003"],
  "attestation": {"sig":"...", "rekor_uri":"..."}
}
```

**Chunking.** FastCDC content-defined chunking (gear-hash, 4 KiB / 64 KiB / 256 KiB) with **forced splits at PG's 8 KiB page boundaries** for heap/index files. Chunk hash = SHA-256 of plaintext; on-disk object = `[12-byte nonce | ciphertext | 16-byte GCM tag]`. Optional per-tenant FastCDC salt to prevent cross-tenant chunk-size fingerprinting (tradeoff: no cross-tenant dedup).

---

## API surface

**REST**, OpenAPI 3.1 at `api/openapi.yaml`, versioned `/v1/`:

```
GET    /v1/healthz                       GET    /v1/metrics
GET    /v1/deployments                   POST   /v1/deployments
GET    /v1/deployments/{d}/backups       POST   /v1/deployments/{d}/backups
GET    /v1/deployments/{d}/backups/{id}  DELETE /v1/deployments/{d}/backups/{id}
POST   /v1/deployments/{d}/backups/{id}/verify
POST   /v1/deployments/{d}/restores      GET    /v1/deployments/{d}/restores/{id}
GET    /v1/deployments/{d}/wal           POST   /v1/deployments/{d}/wal/{seg}/fetch
POST   /v1/deployments/{d}/wal/repair    # recreate slot, resync
POST   /v1/repos/{r}/gc                  GET    /v1/repos/{r}/usage
POST   /v1/kms/rotate                    POST   /v1/kms/shred
GET    /v1/agents                        GET    /v1/audit
GET    /v1/search?q=...
GET    /v1/doctor                        GET    /v1/doctor/{deployment}
```

**gRPC** services in `proto/pg_hardstorage/v1/`: `BackupService`, `RestoreService`, `WALService`, `RepoService`, `KMSService`, `FleetService`, `DoctorService`, `AdminService`. Streaming RPCs for backup/restore progress.

**Auth**: mTLS + OIDC + service tokens, all composable. Tenant-scoped RBAC verbs: `backup:create`, `backup:read`, `restore:execute`, `kms:rotate`, `kms:shred`, `audit:read`, `admin:*`. Default-deny.

---

## CLI surface

```
pg_hardstorage
├── init                          # interactive setup wizard (Day 0)
├── backup        <deployment> [--tag] [--full|--auto] [--from-replica]
├── restore       <deployment> [latest | --to <natlang> | --to-lsn | --to-backup] [--target] [--preview] [--confirm]
├── status        [<deployment>]
├── list          <deployment>
├── show          <deployment> <backup-id>
├── logs          [<deployment>] [-f]
├── doctor        [<deployment>] [--suggest] [--fix]
├── verify        <deployment> [latest|<backup-id>]
├── deployment    add | remove | list | edit | test
├── schedule      <deployment> "every 6 hours" | "0 4 * * *" | off
├── notify        add slack <webhook> | add email | add pagerduty | list | remove
├── rotate        [<deployment>]                 # apply retention now
├── repo          init | check | gc | compact | usage | replicate | set-mode | scrub
├── repair        manifest | chunks | wal | slot | index | attestation | scrub
├── gameday       run | schedule | report     # opt-in chaos automation
├── runbook       generate <deployment> --scenario corruption | dr | upgrade | failover | repo-loss | kms-loss
├── wal           push | fetch | list | repair    # mostly internal
├── logical       add | list | remove | status    # configure logical decoding sinks
├── timetravel    <deployment> --at "<time-or-lsn>"   # ephemeral read-only PG at historical state
├── standby       create | destroy | list           # hot-standby restore (read replicas off backup pipeline)
├── partial       restore <deployment> --tables ... # table-level restore into a running DB
├── hold          add | remove | list               # legal hold
├── classify      <deployment> public|internal|confidential|restricted
├── slo           set | show | report
├── cost          report [--since 30d]
├── capacity      report [--horizon 90d]
├── kms           rotate | shred | inspect | hsm-status
├── audit         search [filters] | verify-chain  # Merkle chain integrity
├── fleet         search --query 'table:public.orders lsn>0/...'
├── server        # run control plane
├── agent         # run agent (Patroni configured per-deployment in YAML)
├── llm           [chat] | ask "<q>" | explain <cmd> | restore <dep> | incident <dep> | runbook <scenario> | postmortem <id> | --mcp-server | --on-error
│                 # plus: skill list | skill show <name> | skill lint <file> | skill test <file> | skill install <name>
│                 #       skill rollback <name> | reload-skills | export-session <id> | show-context
├── completion    bash | zsh | fish
└── version
```

Single config file `pg_hardstorage.yaml` (XDG-discovered) + `--config` override; env vars (`PG_HARDSTORAGE_*`) override file; flags override env. Connection strings follow libpq URI conventions.

The CLI ships **rich progress output** (rate, ETA, dedup ratio, current file) and **a TUI dashboard** (`pg_hardstorage ui`) for live fleet view.

### Output architecture (renderers + sinks, structured-by-design)

This is *not* a "JSON flag bolted on top of `fmt.Println`." Every piece of user-facing output in the system — CLI command results, streaming progress, audit events, alerts, errors, even doctor reports — is a strongly-typed `Event` value flowing through a unified pipeline. Two plugin tiers consume those events:

- **Renderer** — synchronous, command-scoped: takes events, writes bytes to a `Writer` (stdout, stderr, file). Examples: `text`, `json`, `ndjson`, `yaml`, `template`. Future renderers (`csv`, `html`, `pdf-report`, `markdown`) drop in without touching command code.
- **Sink** — asynchronous, system-scoped: takes events, fans out to external systems on its own schedule. Examples: `slack`, `pagerduty`, `webhook`, `email`, `syslog` (RFC 5424), `cef` (ArcSight), `splunk-hec`, `datadog-events`, **`jira`**, `opsgenie`, `servicenow`, `teams`, `opentelemetry-events`. Future sinks drop in the same way.

Both implement small Go interfaces and can be Tier-1 in-tree or Tier-2 `go-plugin` external binaries — same plugin model as Storage / Source / Encryption / Compression. (The original "Notifier" plugin tier collapses into Sink.)

```go
type Severity int8 // RFC 5424 levels: Emerg=0, Alert=1, Crit=2, Error=3, Warn=4, Notice=5, Info=6, Debug=7

type Event struct {
    Schema     string         `json:"schema"`        // "pg_hardstorage.v1"
    Severity   Severity       `json:"severity"`
    SeverityName string       `json:"severity_name"` // "info", "warning", ...
    Component  string         `json:"component"`     // "backup", "wal.stream", "doctor", ...
    Op         string         `json:"op"`            // "backup_started", "progress", "wal_gap_detected"
    Subject    Subject        `json:"subject"`       // {Tenant, Deployment, BackupID, Timeline, ...}
    Body       any            `json:"body"`          // typed payload (per Op)
    Suggestion *Suggestion    `json:"suggestion,omitempty"`
    Trace      TraceContext   `json:"trace,omitempty"`
    GeneratedAt time.Time     `json:"generated_at"`
}

type Renderer interface {
    Name() string
    Render(w io.Writer, ev Event) error
    RenderStream(w io.Writer, evs <-chan Event) error // for backup/restore/verify/logs
    SupportsTTY() bool
    Close() error
}

type Sink interface {
    Name() string
    Open(ctx context.Context, cfg SinkConfig) error
    Emit(ctx context.Context, ev Event) error
    Filter() FilterRule          // severity floor + component allow/deny + rate limit
    Close() error
}
```

### Severity model

RFC 5424-aligned (8 levels) so syslog/CEF emission is direct and lossless:

| Level | Use |
| ---- | --- |
| `emergency` (0) | system unusable — almost never; reserved |
| `alert` (1) | immediate action required (backup repo unreachable) |
| `critical` (2) | repo corruption detected; KMS key destroyed; verification failed catastrophically |
| `error` (3) | a backup failed; restore failed; chunk upload exhausted retries |
| `warning` (4) | WAL lag elevated; anomaly score elevated; retention pruning blocked |
| `notice` (5) | failover handled; slot recreated; verification passed |
| `info` (6) | backup started/completed; WAL segment archived; KMS rotation step |
| `debug` (7) | per-chunk decisions, network retries, plugin invocations |

Sinks declare a severity floor (`emit_severity: warning` only emits warning+); renderers don't filter (the active renderer always renders what the user asked the command to do, regardless of severity).

### Sink configuration (declarative)

```yaml
sinks:
  - name: ops-slack
    plugin: slack
    config:
      webhook_url_secret: kms-secret://ops/slack-webhook
      channel: "#pg-backups"
    filter:
      min_severity: warning
      components: ["backup", "wal.stream", "verify", "kms"]
      rate_limit: { max_per_minute: 10, drop_below: warning }

  - name: incident-jira
    plugin: jira
    config:
      base_url: https://acme.atlassian.net
      project: OPS
      issue_type: Incident
      auth_secret: kms-secret://ops/jira-token
      ticket_strategy: dedupe_by_subject     # update existing ticket for same Subject
    filter:
      min_severity: error
      components: ["backup", "restore", "verify", "kms", "repo"]

  - name: prod-syslog
    plugin: syslog
    config:
      protocol: tls
      address: siem.acme.example.com:6514
      facility: local6
    filter:
      min_severity: notice

  - name: audit-cef
    plugin: cef
    config:
      destination: /var/log/pg_hardstorage/audit.cef
    filter:
      components: ["audit"]                  # everything regardless of severity
```

The `jira` sink (the user's example) creates an incident on `error+`, dedupes via `Subject` (so a recurring failure updates one ticket instead of spawning fifty), and links to the runbook URL embedded in the event's `Suggestion`.

### Renderer details

```
--output text        # default on TTY — human-readable, ANSI colour, ASCII tables
--output json        # default off-TTY — single JSON object (or array)
--output ndjson      # newline-delimited; mandatory for streaming commands
--output yaml        # same schema as JSON, YAML-encoded
--output template    # Go template via --template '{{.deployments[0].rpo_seconds}}'
PG_HARDSTORAGE_OUTPUT=json                   # global override
```

Every command takes `-o` as a short alias.

Future renderers we'll add as the need shows up: `csv` (for fleet exports), `html` (for offline reports), `markdown` (for runbook output), `pdf-report` (compliance), `tap` / `junit` (verifier-as-test-harness). The pipeline is open-ended.

### Stable schema, stable contract

Every event is wrapped as:

```json
{
  "schema": "pg_hardstorage.v1",
  "command": "status",
  "generated_at": "2026-04-28T14:21:08Z",
  "result": {
    "deployments": [
      { "name": "db1",
        "pg_version": "17.2",
        "role": "primary",
        "last_backup": { "id": "db1.full.20260428T0900Z",
                         "completed_at": "2026-04-28T09:12:47Z",
                         "physical_bytes": 13207180000,
                         "verified": true },
        "wal": { "mode": "stream", "lag_seconds": 12, "lag_bytes": 4194304 },
        "rpo_seconds": 2820, "rto_estimate_seconds": 240,
        "health": { "status": "ok", "issues": [] }
      }
    ]
  }
}
```

The `schema` field carries the major-version contract. **24-month backward compatibility** for the JSON schema, matching the on-disk manifest commitment.

### Streaming commands

Backup, restore, verify, WAL stream, logs are NDJSON-native — each line is a typed `Event`. The exact same payload the gRPC streaming RPCs produce, so users can pipe `pg_hardstorage backup db1 -o ndjson | jq` and get the data the REST/gRPC streaming endpoints return.

```
$ pg_hardstorage backup db1 -o ndjson
{"schema":"pg_hardstorage.v1","severity_name":"info","op":"backup_started","subject":{"deployment":"db1","backup_id":"..."}}
{"schema":"pg_hardstorage.v1","severity_name":"info","op":"progress","body":{"bytes_logical":4194304000,"bytes_physical":1342177280,"dedup_ratio":3.12,"throughput_mb_s":620}}
{"schema":"pg_hardstorage.v1","severity_name":"warning","op":"chunker_paused","body":{"reason":"backpressure","stage":"storage_put"}}
{"schema":"pg_hardstorage.v1","severity_name":"notice","op":"backup_completed","body":{"verified":true,"duration_seconds":847}}
```

The same event flow goes to *every* configured Sink concurrently — the user's `ops-slack` and `incident-jira` are notified by the streaming agent with no extra wiring.

### Errors carry remediation

Errors in JSON mode are JSON too — same wrapper, with the `error` field and a structured `Suggestion` so scripts can act on them:

```json
{
  "schema": "pg_hardstorage.v1",
  "severity_name": "error",
  "op": "wal.slot_missing",
  "error": {
    "code": "wal.slot_missing",
    "message": "Replication slot 'pg_hardstorage_db1' is not present on the server.",
    "subject": { "deployment": "db1" },
    "suggestion": {
      "human": "The slot was probably dropped. Recreate it with `pg_hardstorage wal repair db1`.",
      "command": "pg_hardstorage wal repair db1",
      "doc_url": "https://docs.pghardstorage.org/runbooks/wal-slot-missing"
    }
  }
}
```

The same event is what reaches the `jira` sink and becomes the body of a JIRA ticket; the `slack` sink renders a clickable button labelled with `suggestion.human` linking to `suggestion.doc_url`.

### Stable exit codes (also v1 contract)

| Exit | Meaning |
| ---- | ------- |
| 0    | Success |
| 1    | Generic error (with structured `error` payload in JSON mode) |
| 2    | Misuse / bad CLI arguments |
| 3    | Authentication / authorization failure |

```
--output text        # default — human-readable, ANSI colour, ASCII tables
--output json        # single JSON object (or array for list commands)
--output ndjson      # newline-delimited JSON; mandatory for streaming commands
--output yaml        # YAML, same schema as JSON
--output template    # Go template via --template '{{.deployments[0].rpo_seconds}}'
PG_HARDSTORAGE_OUTPUT=json
```

Every command also takes `-o` as a short alias (`-o json`).

**Stable, versioned schema.** Every JSON response is wrapped:

```json
{
  "schema": "pg_hardstorage.v1",
  "command": "status",
  "generated_at": "2026-04-28T14:21:08Z",
  "result": {
    "deployments": [
      { "name": "db1",
        "pg_version": "17.2",
        "role": "primary",
        "last_backup": { "id": "db1.full.20260428T0900Z",
                         "completed_at": "2026-04-28T09:12:47Z",
                         "physical_bytes": 13207180000,
                         "verified": true },
        "wal": { "mode": "stream", "lag_seconds": 12, "lag_bytes": 4194304 },
        "rpo_seconds": 2820, "rto_estimate_seconds": 240,
        "health": { "status": "ok", "issues": [] }
      }
    ]
  }
}
```

The `schema` field carries the major-version contract. We commit to **24-month backward compatibility** of the JSON schema across CLI versions — same window as the on-disk manifest.

**Streaming commands (backup / restore / verify / wal stream / logs)** are NDJSON-only: each line is a typed event. This matches the gRPC streaming-RPC payload exactly so users can pipe `pg_hardstorage backup db1 -o ndjson | jq` and get the same data the REST `/v1/.../backups` streaming endpoint returns.

```
$ pg_hardstorage backup db1 -o ndjson
{"schema":"pg_hardstorage.v1","event":"backup_started","backup_id":"...","started_at":"..."}
{"schema":"pg_hardstorage.v1","event":"progress","bytes_logical":4194304000,"bytes_physical":1342177280,"dedup_ratio":3.12,"throughput_mb_s":620}
{"schema":"pg_hardstorage.v1","event":"chunker_paused","reason":"backpressure","stage":"storage_put"}
{"schema":"pg_hardstorage.v1","event":"backup_completed","backup_id":"...","verified":true,"duration_seconds":847}
```

**Errors in JSON mode are JSON too** — same wrapper, with the `error` field plus a structured `suggestion` (preserving the plain-English remediation directive from the resilience principles):

```json
{
  "schema": "pg_hardstorage.v1",
  "command": "wal stream",
  "error": {
    "code": "wal.slot_missing",
    "message": "Replication slot 'pg_hardstorage_db1' is not present on the server.",
    "deployment": "db1",
    "suggestion": {
      "human": "The slot was probably dropped. Recreate it with `pg_hardstorage wal repair db1`.",
      "command": "pg_hardstorage wal repair db1",
      "doc_url": "https://docs.pghardstorage.org/runbooks/wal-slot-missing"
    }
  }
}
```

Exit codes are stable across output modes (also part of the v1 contract):

| Exit | Meaning |
| ---- | ------- |
| 0    | Success |
| 1    | Generic error (with structured `error` payload in JSON mode) |
| 2    | Misuse / bad CLI arguments |
| 3    | Authentication / authorization failure |
| 4    | Pre-flight check failed (no mutation occurred) |
| 5    | Operation aborted by user (`y/N` declined) |
| 6    | Resource not found (deployment, backup, repo) |
| 7    | Conflict (lease held, in-progress operation) |
| 8    | Storage / KMS unreachable |
| 9    | Verification failure (backup or restore) |
| 10   | Health/`doctor` reports issues (used by `doctor --exit-on-issues`) |

**Auto-detection.** `--output` defaults to `text` on a TTY and to `json` when stdout is not a TTY (pipe / redirect), unless `PG_HARDSTORAGE_OUTPUT` is set. This makes `pg_hardstorage status | jq '.result.deployments'` Just Work without flags.

**`--no-color`, `--quiet`, `--no-progress`** are siblings of `--output` for fine control. `text` mode honours `NO_COLOR` and `CLICOLOR_FORCE` (de-facto cross-tool standards).

**Interactive commands and JSON.** `init` and interactive `restore` refuse `--output json` with exit 2 and a structured suggestion to use the API or non-interactive flags (`pg_hardstorage init --connection ... --repo ... --yes`).

**Why this matters for v0.1.** Designing every command around a typed `Result` value with two renderers (text + JSON) is much cheaper than retrofitting it. Scripts written against v0.1 JSON keep working through v1.0+. Monitoring tools (`pg_hardstorage doctor -o json | …`) and CI pipelines (`pg_hardstorage backup ... -o ndjson | tee log.ndjson`) become first-class consumers from day one.

---

## Multi-instance / fleet model

**One agent per host** (multiplexes all deployments on that host). In K8s, naturally degenerates to "one sidecar per StatefulSet pod." Agents register via mTLS client cert; identity = `(host_fqdn, agent_uuid)`; heartbeat every 10 s.

**Deployment** is the unit: `(pg_connection, repo, retention, schedule, encryption_ref, tenant)`. Bound to ≥ 1 agents (HA). Control plane dispatches `BackupJob` over gRPC, streams progress back.

Configuration **pulled** from control plane on startup; declarative; no agent-side drift.

---

## Verification subsystem

Goroutine pool inside the control plane (also runnable as `pg_hardstorage verify-runner`). Two tiers:

- **Fast verify** (default after every backup, < 60 s): `pg_verifybackup` against the staged manifest. No restore.
- **Full verify** (default weekly, configurable per deployment): allocate sandbox (Docker `postgres:<major>` with tmpfs by default; opt-in Firecracker / k8s `Job` for stronger isolation), `pg_hardstorage restore` into it, run `pg_verifybackup` + `pg_amcheck --all --heapallindexed --rootdescend` + user smoke SQL. Tear down. Append `verification.json` to manifest dir.

Big-DB optimization: full-verify of a 100 TB backup takes hours. Default at this size: **sampled verification** — pick a random 5% of backups per quarter for full verification; everything else gets fast-verify.

Failures emit metric, audit event, and configured notifier (Slack/PagerDuty/email).

---

## Observability + `doctor`

**Prometheus** (namespace `pg_hardstorage_`).  The runtime
registry lives in `internal/obs/metrics/` and serves `/metrics`
in exposition format from the control plane (always) and the
agent (`--metrics-listen`).  The backup, WAL-archive, verify,
KMS, chunk-upload, control-plane-HTTP, job, agent, and
build-info families below are **live**; the SLO / anomaly /
resilience / repo-size / WAL-lag families are **reserved
names** whose producers are still pending (drift #7/#8), for
which `doctor` and the audit log remain the canonical
surfaces.

```
pg_hardstorage_backup_started_total{deployment,type}
pg_hardstorage_backup_completed_total{deployment,type,result}
pg_hardstorage_backup_duration_seconds{deployment,type}      # histogram
pg_hardstorage_backup_bytes_logical{deployment}              # pre-dedup
pg_hardstorage_backup_bytes_physical{deployment}             # post-dedup/compress
pg_hardstorage_backup_dedup_ratio{deployment}
pg_hardstorage_chunk_uploads_total{result}                   # ok|dedup|error
pg_hardstorage_wal_segments_archived_total{deployment,mode}  # mode=stream|library|cmd
pg_hardstorage_wal_archive_lag_seconds{deployment}
pg_hardstorage_wal_archive_lag_bytes{deployment}
pg_hardstorage_repo_objects{repo,kind} pg_hardstorage_repo_bytes{repo,kind}
pg_hardstorage_verify_runs_total{deployment,result,tier}     # tier=fast|full|sampled
pg_hardstorage_kms_unwrap_latency_seconds
pg_hardstorage_anomaly_score{deployment,kind}                # size|churn|duration
pg_hardstorage_agent_up{agent} pg_hardstorage_leader_election_state
pg_hardstorage_rpo_seconds{deployment} pg_hardstorage_rto_estimate_seconds{deployment}
```

**OpenTelemetry**: top-level spans `pg_hardstorage.backup`, `pg_hardstorage.restore`, `pg_hardstorage.wal.archive`, `pg_hardstorage.verify`. Children: `pg.backup_start`, `pg.basebackup.stream`, `chunker.process_file`, `storage.put_chunk` (with `dedup_hit` attr), `kms.unwrap_dek`, `pg.backup_stop`, `manifest.commit`. Trace context propagated agent ↔ control plane.

**Structured JSON logs**, stable keys. Audit events tagged separately and shipped to WORM bucket.

**`doctor`** is the single-command UX for "is everything ok?":

```
$ pg_hardstorage doctor
  db1 — PG 17.2 — primary @ db1.example.com
    ✓ PostgreSQL reachable
    ✓ Replication slot 'pg_hardstorage_db1' active, lag 12s
    ✓ Last backup 47m ago, ✓ verified
    ✓ Repository s3://acme-pg-backups/ writable
    ✓ KMS key reachable
    ✓ Retention applied (12 backups, 142 GB)
    ✓ Schedule: next at 04:00 UTC
    ✓ Disk space on agent host: 38% used
  db2 — PG 16.4 — primary @ db2.example.com
    ✗ Replication slot dropped — agent cannot stream WAL.
      Suggested fix:
        pg_hardstorage wal repair db2
      This will create a new slot and bootstrap from the latest backup's stop_lsn.
    ✗ Last backup 19h ago — overdue (RPO target 6h).
      Suggested fix: triggered automatically once WAL is repaired; or run:
        pg_hardstorage backup db2
  Summary: 1 healthy, 1 needs attention.
```

`--suggest` adds the suggestion text (default on). `--fix` runs the suggested commands after a confirmation prompt. `--json` emits machine-readable form for monitoring integrations.

**Health endpoints**: `/healthz` (liveness), `/readyz` (KMS reachable + repo reachable + leader-elected), `/doctor` (full report as JSON).

---

## Repository structure

```
pg_hardstorage/
├── go.mod, go.sum, Makefile, README.md, LICENSE  (Apache-2.0)
├── cmd/pg_hardstorage/main.go
├── cmd/pg_hardstorage_testkit/main.go        # the testkit binary
├── internal/
│   ├── agent/
│   ├── server/                       # control plane runtime
│   ├── cli/                          # cobra command tree, init wizard, TUI
│   ├── pg/
│   │   ├── basebackup/               # BASE_BACKUP streaming reader
│   │   ├── walreceiver/              # physical streaming replication consumer (PRIMARY WAL PATH)
│   │   ├── logicalreceiver/          # logical decoding consumer (second stream)
│   │   ├── outputplugin/             # pg_hardstorage_proto, pgoutput driver, wal2json driver
│   │   └── libpq/
│   ├── backup/
│   │   ├── orchestrator.go
│   │   ├── manifest.go
│   │   ├── chunker/                  # FastCDC + page-aware splitter
│   │   ├── delta/
│   │   └── retention/                # GFS, simple, count, regulatory
│   ├── restore/
│   │   ├── orchestrator.go
│   │   ├── combine/                  # pg_combinebackup wrapper
│   │   ├── pitr.go
│   │   ├── naturaltime/              # "5 minutes ago" → time.Time
│   │   └── preview.go
│   ├── wal/
│   │   ├── stream/                   # the primary path (single / replica-offload / dual / sync-target / cascading)
│   │   ├── archive/                  # archive_library Unix-socket endpoint
│   │   ├── cmdshim/                  # archive_command shim
│   │   └── audit/                    # gap detector
│   ├── logical/
│   │   ├── orchestrator.go           # per-deployment logical pipelines
│   │   ├── transform/                # PII redaction, column masking
│   │   └── sinks/{chunked,kafka,pubsub,webhook,s3events}/
│   ├── coord/                        # coordination layer abstraction
│   │   # sqlite backend deliberately not present — bookkeeping
│   │   # for single-host runs goes into JSON files at <state>/bookkeeping/
│   │   ├── pgadvisory/               # small fleet via PG advisory locks
│   │   ├── kubelease/                # K8s coordination.k8s.io/Lease
│   │   └── etcd/                     # opt-in for very large fleets
│   ├── timetravel/                   # ephemeral PG from any LSN
│   ├── standby/                      # hot-standby off backup pipeline
│   ├── partial/                      # table-level restore
│   ├── runbook/                      # runbook generator
│   ├── hold/                         # legal hold
│   ├── classify/                     # data classification tags
│   ├── slo/                          # SLO-as-code
│   ├── cost/                         # repo cost reporting
│   ├── capacity/                     # capacity planning
│   ├── repo/
│   │   ├── layout.go
│   │   ├── cas.go                    # content-addressed store
│   │   ├── gc.go                     # mark-and-sweep
│   │   ├── compact.go
│   │   └── replicate.go              # cross-region async copy
│   ├── plugin/
│   │   ├── storage/{s3,fs,azure,azblob,gcs,sftp,scp}/
│   │   ├── encryption/{aesgcm}/
│   │   ├── kms/{awskms,gcpkms,azurekv,vaulttransit,pkcs11}/
│   │   ├── compression/{zstd,none}/
│   │   ├── renderer/{text,json,ndjson,yaml,template,csv,html,markdown,pdf,tap,junit}/
│   │   ├── sink/{slack,pagerduty,webhook,email,syslog,cef,splunkhec,datadog,jira,opsgenie,servicenow,teams,otelevents,discord}/
│   │   ├── llmprovider/{openai,mock}/
│   │   └── goplugin/                 # tier-2 host
│   ├── output/                       # event bus, severity model, dispatcher (renderers + sinks)
│   ├── paths/                        # FHS-aware path resolution: Config / State / Cache / Runtime / Logs
│   ├── llm/
│   │   ├── chat/                     # TUI chat, conversation state, footnoting, hallucination self-check
│   │   ├── tools/                    # tool surface (read_doctor / read_logs / preview_command / execute_command)
│   │   ├── safety/                   # confirmation gates, n-of-m hooks, anomaly refusal
│   │   ├── privacy/                  # PII detector, redaction, mode enforcement
│   │   ├── mcp/                      # MCP stdio + TCP server
│   │   ├── skills/                   # skill loader: schema, signature, RBAC, tool allowlist, hot-reload
│   │   │   ├── builtin/{ask,explain,restore,incident,runbook,postmortem}/    # YAML + .tests.yaml goldens
│   │   │   ├── lint/                 # `pg_hardstorage llm skill lint`
│   │   │   └── test/                 # `pg_hardstorage llm skill test` against pinned model checkpoint
│   │   ├── evidence/                 # signed evidence-bundle exporter
│   │   └── transparency/             # /show-context, /show-tools, /show-skill, /show-budget
│   ├── doctor/                       # health checks + remediation suggestions
│   ├── schedule/                     # built-in scheduler
│   ├── kms/
│   ├── backup/keystore/              # envelope encryption, KEK management
│   ├── fips/                         # BoringCrypto FIPS build variant
│   ├── verify/{runner,sandbox,smoke}/
│   ├── fleet/{registry,scheduler,search,ui}/
│   ├── tenant/                       # tenant model, per-tenant KEK lookup
│   ├── rbac/, audit/
│   ├── api/{rest,grpc}/
│   ├── obs/{metrics,tracing,logging,resilience}/
│   ├── config/, fsutil/, util/
├── proto/pg_hardstorage/v1/, proto/plugin/v1/
├── api/openapi.yaml, api/crd/
├── ext/pg_hardstorage_archive/       # secondary path C extension
├── charts/{pg-hardstorage-server,pg-hardstorage-sidecar}/
├── deploy/{docker,systemd}/
├── scripts/devcluster.sh
├── test/
│   ├── e2e/, chaos/, fuzz/
│   ├── matrix.yaml                            # OS × PG × FS × Patroni × arch matrix definition
│   ├── scenarios/                             # *.scenario.yaml — declarative test scenarios per tier
│   ├── load/                                  # *.load.yaml — deterministic workload definitions
│   └── inventory/                             # SSH inventories for L4/L5 bare-metal runs
├── internal/testkit/                          # implementations live alongside production code
│   ├── topology/{local,kind,k8sremote,sshinventory,cloudvms,firecracker}/
│   ├── load/                                  # deterministic load engine
│   │   ├── prng/                              # chacha20 PRNG, seeded
│   │   ├── ops/                               # per-operation generators
│   │   ├── faker/                             # deterministic data generators
│   │   └── checkpoint/                        # NDJSON checkpoint stream emitter
│   ├── assert/                                # assertion DSL + diffs + page-aware-hash
│   ├── inject/                                # network/disk/mem/proc/pg/k8s/storage/kms fault injectors
│   ├── differential/                          # parity tests vs pgBackRest, WAL-G
│   ├── bisect/                                # scenario-aware git bisect
│   └── matrix/                                # matrix expander + CI tier scheduler
└── docs/
```

---

## Build & distribution

- Static `pg_hardstorage` for `linux/{amd64,arm64}`, `darwin/arm64`, `windows/amd64` (CLI-only on Windows).
- Default `CGO_ENABLED=0`. FIPS variant `pg-hardstorage-fips` (`GOEXPERIMENT=boringcrypto CGO_ENABLED=1`).
- Reproducible builds (`-trimpath -buildvcs=false`, pinned toolchain).
- goreleaser: tarballs + `.deb` + `.rpm` + Homebrew tap + Scoop.
- Container images (distroless): `ghcr.io/cybertec-postgresql/pg_hardstorage:<ver>`, `-fips:<ver>`, `-pg-ext:<pgver>-<hsver>`.
- All artifacts cosign-signed; SBOM via syft; attestations via `cosign attest`.
- Apache 2.0; CLA via DCO sign-off; public plugin registry post-v1.0.

---

## LLM helper — `pg_hardstorage llm`

The 3am operator persona is the design's hardest case, and an LLM is the most leverage we can give them. We treat the LLM as a *grounded assistant* — a chat surface that has read access to the local system state, the runbooks, the docs, the audit log, and (gated) the same tool surface a human operator uses. It is **not** a freeform chatbot dropped on top: every answer is anchored in real state from this specific cluster.

### What "good" looks like at 3am

The operator types `pg_hardstorage llm` after a failure (or the CLI auto-launches it via `--on-error-llm` when a mutating command fails). The assistant opens with the failure already loaded as context and the very first message is structured:

```
[14:23:08] You ran: pg_hardstorage restore db1 --to "5 minutes ago"
           It failed at the WAL replay step with code wal.gap_at_lsn.

I checked your timeline history — there's a gap between LSN 0/3000028 (TLI 2)
and 0/30001A0 (TLI 3) at the failover boundary on 2026-04-28 09:12.

Three options, in order of likelihood:
 1. Restore to LSN 0/3000028 (just before the gap) — safe, you lose ~3 minutes
    of writes that didn't make it across the failover.
 2. Restore to LSN 0/30001A0 (just after the gap) on TLI 3 — same ~3 minutes
    lost but on the post-failover timeline; preferred if your application has
    already adapted to the new TLI.
 3. Investigate the gap (we may have WAL on the rewound primary) — I can run
    `pg_hardstorage repair wal --gaps --source node-A` to attempt recovery.

Which would you like to try? (I can also explain any of these in more detail,
or pull up runbook R6 for context.)
```

That happens because the assistant has the doctor output, the audit trail of the failed command, and runbook R6 already in its context window before the user types a thing.

### Architecture: LLM as a plugin tier

`LLMProvider` is a new plugin tier alongside Storage / Source / Encryption / Compression / Renderer / Sink. Same Tier-1 / Tier-2 model.

```go
type LLMProvider interface {
    Name() string
    Open(ctx context.Context, cfg ProviderConfig) error
    Chat(ctx context.Context, msgs []Message, tools []ToolDef) iter.Seq2[ChatChunk, error]
    SupportsTools() bool
    SupportsStreaming() bool
    Close() error
}
```

Tier-1 providers (in-tree, swap with one config line):

- **`openai`** — OpenAI / Azure-deployed OpenAI APIs.
- Providers currently implemented: `openai`, `mock`. Additional providers (bedrock, vertex, ollama, llama-cpp, huggingface) are planned for later milestones.

Tier-2 plugins via `go-plugin` for vendors we haven't covered.

### MCP (Model Context Protocol) — first-class

Many operators already have a preferred LLM client. We expose `pg_hardstorage` as an **MCP server** so any MCP-aware client (Continue, Cursor, Zed, Goose, Cline, …) connects natively without us hosting a chat UI:

```
$ pg_hardstorage llm --mcp-server                # stdio MCP server
$ pg_hardstorage llm --mcp-server --tcp :7099    # TCP variant
```

The operator adds `pg_hardstorage` to their MCP-aware client's config; the client speaks to it over stdio. Their preferred LLM, their preferred client, our tools. This is the highest-leverage integration we can ship — we don't compete with the LLM-client market, we plug into it.

### Tool surface exposed to the LLM (read-only by default)

Every tool the LLM can call is a thin wrapper around our existing structured-output APIs — no new code paths, no parallel implementations.

```
read_doctor(deployment?) -> doctor JSON
read_status(deployment?) -> status JSON
read_logs(deployment?, since?, min_severity?) -> recent events (NDJSON)
read_backup(deployment, backup_id) -> manifest (sensitive bits redacted)
read_wal_inventory(deployment) -> WAL segments + timeline history
read_repo_usage(repo?) -> sizes, retention state
read_runbook(scenario) -> one of R1–R7 (or generated)
read_audit(filters) -> audit log entries (RBAC-scoped)
read_config() -> redacted config (secrets masked)
search_docs(query) -> semantic-search hits over /usr/share/pg_hardstorage/
search_fleet(query) -> fleet-wide backup search
preview_command(cmd) -> dry-run preview JSON (the same --preview output a human sees)
suggest_command(cmd, why) -> renders a confirmation block to the user; never executes
```

### Mutating tools (gated, opt-in, audited)

In `--mode advise+execute` (off by default; opt-in flag in v1.0), the LLM can additionally call:

```
execute_command(cmd) -> runs the command after explicit user confirmation
```

But the safety stack around `execute_command` is non-negotiable:

1. **The LLM never bypasses RBAC.** It runs as a real principal with a real token. Tokens are JIT, time-boxed, and scoped to the operator's permissions — the LLM can't ask for more than the human could do directly.
2. **Pre-flight `--preview` first** for every suggested mutation. If preview fails, we never reach execute.
3. **Confirmation block** in the chat — exact command, what changes, RTO/blast-radius estimate, big visible YES/NO. The LLM cannot fake this; it's rendered by our code, not by the model.
4. **Type-the-command for the most destructive ops.** `kms shred`, `repo gc --delete`, `backup delete --force` require the user to literally type the command name to confirm.
5. **n-of-m approval** still applies. The LLM can invite a second approver via the configured Sinks (drop a Slack message, open a Jira ticket).
6. **Anomaly refusal.** If the LLM proposes a command that's wildly inconsistent with the just-prior context (e.g. user asked about restore, LLM proposes `kms shred`), the safety layer refuses with a structured event, audited.
7. **Full audit.** The entire conversation is logged as audit events: prompts, tool calls, responses, executed commands. Same WORM-bucket destination as everything else. Post-incident review can replay the whole 3am session.

### Privacy modes (default-conservative)

```yaml
llm:
  provider: openai
  privacy: standard      # one of: strict | standard | open | local-only
```

- **`strict`** — only error codes, metric names, and runbook IDs cross the boundary. No deployment names, no LSNs, no error message strings. Useful for regulated environments where the LLM provider is treated as untrusted.
- **`standard`** (default) — metadata, doctor JSON, error messages, redacted config. PII detector strips obvious patterns (emails, IPs, connection strings) before sending.
- **`open`** — everything goes (with credentials always masked). For dev / staging.
- **`local-only`** — refuses any provider that's not local. Hard gate. Auto-selected when the deployment has `data_classification: confidential` or higher.

### API-key custody

LLM provider credentials get the same envelope-encrypted treatment as everything else:

```yaml
llm:
  provider: openai
  api_key_secret: kms-secret://prod/llm/openai   # wrapped via the configured KMS
```

Or via cloud IAM (no key at all): `bedrock` uses the agent's AWS role, `vertex` uses ADC. For local models there's no secret to keep.

For self-managed paths the user picks: system keyring (libsecret on Linux, Keychain on macOS, Credential Manager on Windows), or the same `/etc/pg_hardstorage/keyring/` mode-0700 directory the rest of the keys live in.

### Cost and rate controls

- Per-session token budget; per-day budget per principal.
- Pre-flight context-size estimate before calling; truncation strategy that preserves the most-recent doctor output and the failing event.
- Tier-aware model selection: cheap model for routine questions ("explain this error code"), strong model only when the user explicitly escalates ("walk me through this restore").
- Streaming responses so the user can interrupt without paying for a full completion.
- A daily summary metric: `pg_hardstorage_llm_tokens_total{provider,principal,direction}`, cost dashboard reports via the existing Cost reporter.

### Skills are *files*, not hardcoded code paths

The skills are not Go functions baked into the binary. They are versioned, declarative **skill files** (YAML) loaded at runtime from a precedence chain. This is a deliberate architectural choice — the skills are exactly the thing that needs to evolve fastest as we encounter new failure modes, and they shouldn't require a binary release to fix.

```
/usr/share/pg_hardstorage/skills/                # shipped skills (read-only, package-owned)
  restore.skill.yaml
  incident.skill.yaml
  ask.skill.yaml
  explain.skill.yaml
  runbook.skill.yaml
  postmortem.skill.yaml
/etc/pg_hardstorage/skills/                      # operator overrides + custom skills
  acme-restore.skill.yaml
~/.config/pg_hardstorage/skills/                 # user-private skills
```

A skill file looks like:

```yaml
schema: pg_hardstorage.skill.v1
name: restore
display_name: Restore Wizard
version: 1.4.2
description: |
  Walks the user through restore decisions: pick a backup, target, time,
  verify pre-flight checks, then ask for confirmation. Never proposes a
  mutation without preview_command first.
trigger:
  manual: ["pg_hardstorage llm restore"]
  auto_on_error: ["restore.failed", "wal.gap_at_lsn", "pg.basebackup_disconnected"]
permissions:
  read_only: true
  required_rbac: ["backup:read", "restore:read"]   # MUST hold these to invoke this skill
context:
  preload_tools:
    - read_doctor
    - read_status
    - read_wal_inventory
    - read_runbook: { id: R5 }
  available_tools:
    - read_doctor
    - read_status
    - read_backup
    - read_wal_inventory
    - read_runbook
    - search_fleet
    - preview_command          # MANDATORY for any suggested command
    - suggest_command          # always available
    # explicitly NOT in this list: execute_command, kms_*, repo_gc, backup_delete
guardrails:
  - max_token_budget_per_session: 80000
  - max_tool_calls_per_turn: 8
  - require_preview_before_suggest: true
  - refuse_if_classification_above: confidential   # this skill not approved for top-secret data
  - mandatory_disclaimer: "AI assistant — every suggested command must be verified by you before running."
prompt_template: |
  You are the restore-wizard skill for pg_hardstorage version {{ .BinaryVersion }}.
  Cluster: {{ .DeploymentSummary }}.
  Recent failure context: {{ .FailureContext }}.

  Hard rules:
   1. Never suggest a command you have not first run through preview_command.
   2. Always present the user with a numbered list of options including doing nothing.
   3. Cite every factual claim with the tool call that produced it (footnote syntax: [tool:N]).
   4. If you are uncertain, say so plainly and offer to escalate_to_human.
   5. Refuse to discuss any data outside the scope of the current deployment.
post_session:
  emit_audit: skill.restore.completed
  request_feedback_prompt: "Was this helpful? (1-5)"
signature: cosign://signatures/restore.skill.yaml.sig
```

The implications:

- **Hot-fix loop in minutes, not weeks.** A bad skill response in production gets a same-day patch — drop a new YAML file in `/etc/pg_hardstorage/skills/`, increment the `version`, restart the agent (or `pg_hardstorage llm reload-skills`). No binary rebuild, no Debian package release.
- **Skill isolation.** A bug in the postmortem skill cannot touch the restore skill. Each skill loads independently, has its own tool allowlist, its own guardrails, its own RBAC scope.
- **Skill linting + golden tests.** `pg_hardstorage llm skill lint <file>` validates the schema and static-checks the tool list (no banned tools, no missing required ones). `pg_hardstorage llm skill test <file>` runs golden test cases (`<file>.tests.yaml`) — given a frozen cluster state and a user prompt, the LLM with this skill must produce a response satisfying these assertions. CI runs these against a pinned model checkpoint per release.
- **Skill versioning + bisect.** Every skill carries a SemVer. `pg_hardstorage llm skill rollback restore` reverts to the previous version of just that skill. Per-skill bisection across an incident window is one command.
- **Signed skills.** Shipped skills are cosign-signed by the project key; user-added skills are signed by the operator's local cosign key (or signed-by-config-flag). Loading an unsigned skill emits a critical audit event and requires `--allow-unsigned-skill` confirmation. This is the same trust posture as everything else in the binary.
- **Marketplace path.** Skills are YAML; we can publish a community registry of skills at `registry.pghardstorage.org/skills/` post-v1.0. Operators can `pg_hardstorage llm skill install <name>` from the registry, with cosign signature verification by default.

The default skills (restore, incident, ask, explain, runbook, postmortem) ship as YAML inside the binary's package — they are the same shape an operator-written skill would be, no privileged code path.

### Verify-before-execute is structural, not optional

For every suggested command:

```
LLM proposes -> preview_command runs (always) -> result rendered to user
            -> user types y/N (or types the command name for high-risk ops)
            -> if y: execute_command runs (only available in advise+execute mode)
                  -> RBAC check (token must hold the verb)
                  -> n-of-m approval if configured for this op
                  -> command runs
                  -> result rendered + emitted to all configured Sinks
```

There is **no path** by which the LLM's textual suggestion turns into an executed command without `preview_command` succeeding and the user explicitly confirming. The LLM cannot construct strings that bypass this — `execute_command` validates that the *exact* string was just shown by `preview_command` in this same turn (sliding-window match, replay-protected). If preview is stale or absent, execute refuses.

For destructive operations (`kms shred`, `repo gc --delete`, `backup delete --force`, `repo wipe`), confirmation is **typed**, not pressed. The user must type the literal command string. The LLM cannot type for them.

### Accountability & evidence ("if it goes wrong, the audit shows what happened")

The non-negotiable goal: if a backup is lost or a restore fails after an LLM-assisted operation, the audit trail must show **exactly** who saw what, who decided what, and what the system did — with cryptographic evidence. The LLM is a tool; the human operator is the actor; the binary is the executor; all three actions are independently verifiable.

What we log for every LLM session:

| Captured | Where |
| --- | --- |
| Every prompt sent to the model (post-redaction, full text under privacy mode) | Audit event `llm.prompt`, hash-chained into the Merkle audit log |
| Every tool call the model made + its arguments + its result | `llm.tool_call`, hash-chained |
| Every model response in full | `llm.response`, hash-chained |
| Every command the model suggested + the preview output | `llm.suggestion` |
| Every confirmation the user gave (with the literal text typed) | `llm.confirmation` |
| Every executed command, its result, exit code | `llm.execution` (and the standard execution audit event too) |
| The skill file (path + version + cosign signature ref) used in the session | `llm.skill_used` |
| The model provider, model id, model version | `llm.model_used` |
| The operator principal, tenant, RBAC scope at session start | `llm.session_started` |
| Token usage per turn | `llm.tokens` |
| Hand-offs to human (Jira ticket id, Slack thread url) | `llm.escalated` |

All of these append to the same hash-chained Merkle audit log used for everything else, sink-fanned-out to every configured Sink, and (when WORM is configured) anchored to S3 Object Lock and a transparency log (Rekor) on the standard cadence. The chain is verifiable post-hoc with `pg_hardstorage audit verify-chain --since <ts>` — tamper-evident.

The user can produce a **signed evidence bundle** for any session:

```
$ pg_hardstorage llm export-session <session-id>
Wrote signed evidence bundle to ./session-20260428T1423-db1-restore.evidence.tar.gz
  - transcript.ndjson         (every prompt, tool call, response, in order)
  - tool_results/             (raw JSON of each tool call's return)
  - executed_commands.ndjson  (every command actually run, exit code, duration)
  - audit_chain_proof.json    (Merkle proof: this session's events anchor at chain pos 1428..1547)
  - skill_used.yaml           (the exact skill file at the version used)
  - skill_signature.sig       (cosign signature)
  - model_metadata.json       (provider, model id, model version, model fingerprint)
  - signature.sig             (cosign signature on the bundle)
```

This bundle is what an admin shows in a post-incident review or a regulatory audit. Independent verifiability; no trust required in our software's good-faith reporting.

### Operator transparency at runtime

At any moment during a session the operator can run:

```
> /show-context           # what the model has seen so far this session
> /show-tools             # which tools are available in this skill
> /show-skill             # which skill is active, version, file path, signature status
> /show-budget            # tokens used, tokens remaining, cost so far
> /redact <field>         # mark a field as do-not-send for the rest of the session
> /escalate-to-human      # trigger Sink fan-out, attach transcript
> /export                 # save this session as a signed evidence bundle
```

Nothing is hidden. The LLM has no privileged state the operator cannot inspect.

### Disclaimers visible at all times

The TUI top bar shows:

```
[AI assistant · skill=restore v1.4.2 · provider=openai · privacy=standard ·
 every suggestion must be verified by you before execution]
```

The first response of every session also includes a one-line reminder. The CLI exit screen shows session metadata and where the evidence bundle is, if requested. We are loud about the fact that the LLM is advisory, never authoritative.

### What this defends against

- **"The LLM told me to."** The audit shows it didn't *tell* you — it *suggested*, you *confirmed*, the system *executed*. Three independent decisions, each cryptographically witnessed.
- **"The LLM lied about cluster state."** Every factual claim is footnoted; the bundle has the raw tool-call results to compare.
- **"You released a bad model update."** The bundle records the exact model id and version. Provider-side model versioning is captured.
- **"The skill was malicious."** Skills are signed; unsigned skills require a flag whose use is audited.
- **"You hid prompts from me."** `/show-context` plus the export bundle prove otherwise.

This is the kind of accountability the rest of the system already has for backups (cosign + Rekor + WORM); we extend the same posture to the LLM surface so a regulatory auditor sees no asymmetry.

---

### Pre-defined sessions / skill modes

```
pg_hardstorage llm                                     # interactive chat, read-only
pg_hardstorage llm --on-error                          # auto-launches with the failed-cmd context loaded
pg_hardstorage llm restore <deployment>                # restore-decision skill: pick backup, target, time
pg_hardstorage llm incident <deployment>               # incident-response skill: gather state, draft report
pg_hardstorage llm explain <command>                   # explain a CLI invocation; no tool calls; cheap
pg_hardstorage llm ask "<question>"                    # one-shot question, exit-on-answer; pipe-friendly (-o ndjson)
pg_hardstorage llm runbook <scenario>                  # generate a bespoke runbook for this cluster
pg_hardstorage llm postmortem <incident-id>            # synthesize a postmortem from audit log + chat history
pg_hardstorage llm --mcp-server                        # MCP stdio server for external clients
```

> **GUI is out of scope for pg_hardstorage.**  This binary is
> command-line only.  The richer browser-based UI is a separate
> commercial product with its own repository and release cadence
> — it consumes the same v1-stable JSON / NDJSON APIs the CLI
> emits, plus the gRPC + REST endpoints the control plane
> exposes.  No `--web` flag will ship in this binary.

### Hand-off to human

When the LLM is uncertain, it has a tool: `escalate_to_human(channel, summary)`. That triggers our existing Sink fan-out — Jira ticket created with the conversation transcript, Slack thread opened, PagerDuty page raised — same primitives as automated alerting, just initiated by the assistant. The human picks up the thread; the conversation history follows.

### Self-checks against hallucination

- Every command the LLM suggests is **rendered through `preview_command` first**. If preview fails (command wouldn't execute, or refers to a nonexistent deployment), the suggestion is silently retried at most twice; on third failure the LLM tells the user it cannot find a valid command and proposes asking a human.
- Every claim the LLM makes about cluster state is **footnoted with the tool call that backed it**. The TUI renders these as small superscripts (`[doctor:1]`, `[runbook:R6]`) the user can expand to see the underlying JSON.
- A "verify with primary source" toggle: in skeptical mode, the LLM is forced to run the corresponding tool before any factual claim about the cluster.

### Sample interactions

```
> Why did my last backup fail?
< Looking at your audit log… your most recent backup attempt for db1 was at 14:09 UTC.
  It failed at the BASE_BACKUP step with `pg.basebackup_disconnected`. Your replication
  user 'pg_backup_user' lost connection 47 seconds in. Looking at the WAL stream events
  around that time, the primary was restarted (likely a Patroni-initiated restart). The
  backup orchestrator did the right thing — aborted, did not commit a partial manifest.
  The next scheduled backup at 18:00 UTC will run normally. You don't need to do
  anything, but if you want a backup right now, run `pg_hardstorage backup db1`.

> Are my backups GDPR-shred-ready?
< Yes — your deployment db1 uses tenant 'default' with KEK at aws-kms://…/abc123. To
  shred a specific tenant, run `pg_hardstorage kms shred --tenant <id>`. You currently
  have one tenant; if you want per-customer crypto-shred, you'll need to migrate to
  a multi-tenant configuration. I can show you how — should I?

> WAL lag is 47 minutes. What now?
< I'm running doctor for you… the slot is intact, lag is climbing, and the agent log
  shows a sustained S3 5xx storm for the last 51 minutes (47 successful retries).
  The circuit breaker hasn't tripped yet. Two paths:
   1. Wait — circuit breaker will trip at 60 minutes, automatically failing over to
      replica region. Cleaner; about 13 minutes to wait.
   2. Manually fail over now: `pg_hardstorage repo replicate --activate-secondary`.
      Riskier if the secondary region is also affected; let me check it.
  [check_secondary_region]: secondary region S3 is healthy.
  Recommend option 2 if you can't afford another 13 minutes of WAL lag. Should I run it?
```

### Phasing

- **Current** — read-only LLM helper. Providers: `openai`, `mock`. MCP server. TUI chat. Privacy modes. Audit integration. Pre-defined skills: `ask`, `explain`, `restore`, `incident`, `runbook`, `postmortem`. `--on-error-llm` auto-launch. Token / cost metrics. Advise+execute mode (gated, opt-in) with confirmation/RBAC/n-of-m flow.
- **Planned** — Additional providers (bedrock, vertex, ollama, llama-cpp, huggingface). Localization in the assistant (German, French, Japanese). Persistent conversation history (encrypted, per-user, per-deployment). Community skill registry. Skill versioning + per-skill rollback.

### How good can we make it?

Honestly: **better than docs, better than chatops, on par with a senior DBA for the most common 80% of incidents** — *because* it's grounded in the cluster's real state, its runbooks, its audit log, and the structured-output APIs we already have. The places it will struggle: novel data-corruption scenarios that aren't in the runbooks, vendor-specific managed-DB quirks the doc corpus doesn't cover, business-context decisions that need a human ("should we accept 3 minutes of data loss?"). For those it has to know to escalate, and the safety stack ensures it can't bluff into doing damage.

The biggest design discipline: **tools first, prompts second**. The strength of this assistant comes from what it can *see* and *do*, not from how clever the prompt is. Every time someone asks for a feature, the right question is "is there a tool the LLM should call?" and only then "what should the prompt say?".

---

## Filesystem layout & OS packaging (FHS-clean, Debian-ready)

The design is already FHS-compatible: every concern (config, state, runtime, logs, cache, runbooks, units) lives in a logically separate location *in the code*, with paths resolved at startup by a small `paths` package. Nothing in the binary hardcodes "everything is in one directory." That makes Debian packaging straightforward and lets the same binary live happily under either layout.

### Standard FHS path scheme (Debian + Fedora/RHEL alike)

```
/usr/bin/pg_hardstorage                                     # the binary (single static)
/usr/bin/pg-hardstorage-cluster                             # debian-style wrapper (see below)
/usr/lib/systemd/system/pg_hardstorage.service              # default unit
/usr/lib/systemd/system/pg_hardstorage@.service             # templated unit for multi-instance
/usr/share/pg_hardstorage/runbooks/                         # bundled disaster runbooks (R1–R7)
/usr/share/pg_hardstorage/completions/{bash,zsh,fish}/      # shell completions
/usr/share/pg_hardstorage/openapi.yaml
/usr/share/pg_hardstorage/crd/                              # Kubernetes CRDs
/usr/share/doc/pg_hardstorage/                              # docs (Debian-style README, NEWS, changelog.gz)
/usr/share/man/man1/pg_hardstorage.1.gz                     # manpages

/etc/pg_hardstorage/pg_hardstorage.yaml                     # main config
/etc/pg_hardstorage/conf.d/*.yaml                           # drop-in snippets (Debian + Fedora idiom)
/etc/pg_hardstorage/deployments/<name>.yaml                 # per-deployment configs
/etc/pg_hardstorage/sinks/<name>.yaml                       # per-sink configs
/etc/pg_hardstorage/keyring/                                # passphrase wrappers (mode 0700, owner pgbackup)

/var/lib/pg_hardstorage/                                    # mutable state (mode 0750, owner pgbackup:pgbackup)
/var/lib/pg_hardstorage/bookkeeping/                        # per-package JSON state (single-host coordination)
/var/lib/pg_hardstorage/inflight/                           # mmap'd inflight buffers (resumable ops)
/var/lib/pg_hardstorage/crashes/                            # panic-capture bundles
/var/cache/pg_hardstorage/                                  # bloom filters, manifest indexes (rebuildable)
/var/log/pg_hardstorage/                                    # only when journald is unavailable
/run/pg_hardstorage/                                        # runtime sockets (archive_library Unix socket)
/run/pg_hardstorage/archive-<cluster-id>.sock               # per-PG-cluster archive socket
```

Each path is resolved via the precedence chain: explicit flag > env (`PG_HARDSTORAGE_*_DIR`) > XDG (when running as a user) > FHS defaults.

### Why this is naturally Debian-compatible

- **Strict separation of concerns** in the code (`paths.Config()`, `paths.State()`, `paths.Cache()`, `paths.Runtime()`, `paths.Logs()`) means the same binary respects whatever layout the OS gives it. The Debian `.deb` and the Fedora `.rpm` differ only in their packaging metadata — no code differs.
- **No "/opt-style" assumption.** We never write code that assumes a single directory holds everything. A user can still *choose* a single-directory layout (see "RHEL-style consolidation" below), but the binary doesn't depend on it.
- **journald-first logging.** systemd's journald is the default sink for stdout/stderr; we fall back to `/var/log/pg_hardstorage/` only when journald is missing. This is the modern Debian/Fedora norm.
- **Drop-in config (`conf.d/`).** Both Debian and Fedora lean on `conf.d` for layered configuration; we honour it natively.
- **Multi-instance via systemd templates.** `pg_hardstorage@db1.service` and `pg_hardstorage@db2.service` instantiate from `pg_hardstorage@.service`. This matches PostgreSQL's own `postgresql@<ver>-<cluster>.service` convention exactly, so admins already know the muscle memory.

### The Debian-style "unified view" wrapper

The user observation is fair: RHEL/RPM packages often (especially for commercial DBs) bundle config + data + logs under one tree like `/opt/<vendor>/<app>/`, and that single-directory feel is convenient. Debian splits across `/etc`, `/var/lib`, `/var/log`, etc. The way Debian's `postgresql-common` solves this for PG itself is the wrapper-tooling pattern (`pg_ctlcluster`, `pg_lsclusters`, `pg_createcluster`).

We adopt the same pattern. **`pg-hardstorage-cluster`** (shipped by the `pg-hardstorage-common` package on Debian) is a thin wrapper that:

```
$ pg-hardstorage-cluster ls
 NAME    STATE    CONFIG                                       DATA                              SOCKET
 db1     active   /etc/pg_hardstorage/deployments/db1.yaml     /var/lib/pg_hardstorage/db1       /run/pg_hardstorage/archive-db1.sock
 db2     active   /etc/pg_hardstorage/deployments/db2.yaml     /var/lib/pg_hardstorage/db2       /run/pg_hardstorage/archive-db2.sock
 staging stopped  /etc/pg_hardstorage/deployments/staging.yaml /var/lib/pg_hardstorage/staging   —

$ pg-hardstorage-cluster create db3 --connection postgres://...
$ pg-hardstorage-cluster start db3
$ pg-hardstorage-cluster status db3
$ pg-hardstorage-cluster edit db3       # opens db3.yaml in $EDITOR
$ pg-hardstorage-cluster log db3 -f     # journalctl -u pg_hardstorage@db3.service -f
$ pg-hardstorage-cluster purge db3      # removes config, state, runtime; refuses if backups exist
```

It abstracts the path scatter. Users who want it never have to remember `/etc` vs `/var/lib`. Users who prefer to interact directly with FHS paths can — both work. We borrow the verb naming from PostgreSQL's own tools so the muscle memory transfers.

### RHEL-style consolidation (opt-in)

If a user genuinely wants RHEL-style "everything under one tree" (say, on an air-gapped appliance, or in a shared `/opt` mount), they can:

```yaml
# /etc/pg_hardstorage/pg_hardstorage.yaml
paths:
  root: /opt/pg_hardstorage      # all of: config-overlay, state, cache, logs, runtime under here
```

The `paths` package then resolves all subdirectories under `root`. systemd unit gets `Environment=PG_HARDSTORAGE_ROOT=/opt/pg_hardstorage` and the OS-default paths are unused. SELinux/AppArmor profiles are shipped for both layouts.

### Debian packaging skeleton

Multiple binary packages from a single source:

```
pg-hardstorage                    # the binary + completions + manpage (Depends: pg-hardstorage-common)
pg-hardstorage-common             # shared data: runbooks, openapi.yaml, CRDs, the wrapper script
pg-hardstorage-fips               # FIPS variant binary (Conflicts: pg-hardstorage)
pg-hardstorage-pg-ext-15          # archive_library .so for PG 15
pg-hardstorage-pg-ext-16          # ditto for PG 16
pg-hardstorage-pg-ext-17          # ditto for PG 17
pg-hardstorage-server             # control plane variant (Recommends: postgresql for advisory-lock coordination)
```

The `debian/` directory:

```
debian/control                    # source + binary package metadata, dependencies
debian/changelog                  # auto-generated from git tags
debian/copyright                  # Apache-2.0 + DEP-5 machine-readable
debian/rules                      # debhelper compat 13; uses dh --with=systemd,golang
debian/pg-hardstorage.install     # files → paths
debian/pg-hardstorage.service     # → /lib/systemd/system/pg_hardstorage.service
debian/pg-hardstorage.postinst    # adduser pgbackup; mkdir state dirs; systemctl daemon-reload
debian/pg-hardstorage.prerm       # systemctl stop on removal
debian/pg-hardstorage.postrm      # purge config (only on `dpkg --purge`)
debian/pg-hardstorage.lintian-overrides  # documented exceptions only; aim is lintian-clean
debian/pg-hardstorage.docs        # README, ARCHITECTURE.md
debian/pg-hardstorage.manpages    # man/*.1
debian/pg-hardstorage.bash-completion
debian/pg-hardstorage.dirs        # /etc/pg_hardstorage/conf.d, /var/lib/pg_hardstorage, ...
debian/source/format              # 3.0 (quilt)
```

Conformance gates we hold ourselves to:

- `lintian -i -E -I` clean (we may declare overrides only with documented justification).
- `dpkg --verify pg-hardstorage` clean after install.
- `piuparts` (install/upgrade/purge cycle) clean.
- Reprotest (reproducible builds) green.
- Debian Policy Manual compliance.
- `apt full-upgrade` from N-1 to N preserves all state.
- `debconf` for first-run guidance: postinst can prompt for `dpkg-reconfigure pg-hardstorage` to walk through `pg_hardstorage init`.

### RHEL/Fedora packaging skeleton

Equivalent split into RPMs (`pg_hardstorage`, `pg_hardstorage-common`, `pg_hardstorage-fips`, `pg_hardstorage-pg-ext-{15,16,17}`, `pg_hardstorage-server`), with:

- `pg_hardstorage.spec` Fedora Packaging Guidelines compliant.
- `%check` runs the unit tests.
- `%pre` / `%post` / `%preun` / `%postun` mirror the Debian maintainer scripts.
- SELinux policy ships in `pg_hardstorage-selinux` subpackage.
- Submitted to EPEL / Fedora repos following standard review.

### Verifying packageability ahead of time

We don't wait until v1.0 to discover packaging surprises. CI runs:

- `dpkg-buildpackage` against latest stable Debian + the next release in `sbuild` chroots.
- `rpmbuild` against current Fedora + RHEL 9.
- `lintian` on the Debian artefacts; `rpmlint` on the RPMs.
- `piuparts` install/upgrade/purge cycle.
- A "fresh-vm" smoke test: provision a vanilla Debian VM, `apt install ./pg-hardstorage_*.deb`, run `pg_hardstorage init` non-interactively, take a backup against a freshly-provisioned local PG, restore it, verify, uninstall, assert no leftover files outside `/var/lib/pg_hardstorage` (and that the user has been told to keep it for backup retention).

Current packaging ships hand-rolled `.deb`/`.rpm` via goreleaser. Distro-native packaging (lintian-clean Debian source package + Fedora/RHEL spec) is planned.

---

## Test infrastructure — `pg_hardstorage testkit`

Testing this kind of system properly means reproducing realistic fleet topology, exercising every supported (OS × PG × filesystem × Patroni × arch) cell, generating workload that's deterministic enough to assert "yes, at 22:00:34 UTC the database had exactly 1,000,000 rows in `users` and a content digest of `md5:abc123…`", and injecting failures at every layer. We ship this as a *first-class component* — `pg_hardstorage testkit` — under the same Apache 2.0 license. Internal teams, fleet operators, and downstream packagers all use it for their own validation; we use it ourselves for CI.

### Design principles

1. **Determinism is non-negotiable.** Same seed + same scenario → same bytes, same row counts, same LSN advance, same checksums. Without this, "did the restore work?" has no answer.
2. **Scenarios are files**, not Go test cases. YAML-defined, schema-versioned, hot-reloadable, dry-runnable. Same posture as Skills.
3. **Topology is a backend.** K8s, SSH, local Docker, cloud VMs are interchangeable topology providers behind one interface.
4. **Tiered CI cost.** Lvl-1 smoke runs on every PR in 5 minutes; Lvl-5 pre-release covers the full matrix overnight. Each tier's scope is declared in the scenario file, not in CI YAML.
5. **Reproducibility over coverage.** A bug report compiles to a scenario file the user can hand back; we re-run it and reproduce the failure exactly.

### `pg_hardstorage testkit` binary

A separate static binary (built from the same repo, sharing internal packages) so testing dependencies don't bloat the production binary. Lives at `cmd/pg_hardstorage_testkit/`.

```
pg_hardstorage_testkit
├── scenario     run | list | lint | bisect | reproduce
├── load         run | verify | checkpoint show
├── matrix       expand | run | report
├── topology     up | down | list | export    # bring up k8s/SSH/local/cloud topologies
├── inject       network | disk | mem | proc | pg | k8s
├── differential run --against pgbackrest|walg
├── coverage     report --by code-path | matrix-cell | scenario
└── completion
```

### Topology providers

The same scenario runs against any of these — the topology block in the scenario picks the backend.

| Provider | Use case | Speed | Realism |
| --- | --- | --- | --- |
| **`local-docker`** | Laptop dev, fast iteration | seconds | low — single-host PG |
| **`testcontainers`** | Per-PR unit & integration | seconds | low — single-host PG |
| **`kind`** | K8s scenarios on a CI runner | ~2 min boot | medium — single-node K8s |
| **`k8s-remote`** | Tests against a real shared CI K8s cluster (CloudNativePG / Zalando / Crunchy) | seconds (no boot) | high — operator-managed multi-node |
| **`ssh-inventory`** | Real Linux distros + real PG packages | minutes | very high — actual distro paths, systemd, FHS |
| **`cloud-vms` (terraform)** | Real EC2/GCE/Azure with real EBS/PD/Managed-Disk snapshots | minutes-hours | highest |
| **`firecracker`** | Multi-kernel-version coverage | seconds | medium — minimal microVM |

The SSH inventory format mirrors Ansible — operators with existing fleets can point us at theirs:

```yaml
inventory:
  hosts:
    - { host: pg15-deb12.lab,  user: root, os: debian-12,  pg: "15.5", fs: ext4,  arch: amd64 }
    - { host: pg16-rocky9.lab, user: root, os: rocky-9,    pg: "16.4", fs: xfs,   arch: amd64 }
    - { host: pg17-zfs.lab,    user: root, os: ubuntu-24.04, pg: "17.2", fs: zfs, arch: arm64 }
    - { host: pg17-pat.lab,    user: root, os: alma-9,     pg: "17.2", fs: ext4,  arch: amd64, patroni: 3.3 }
  defaults:
    ssh_key: ~/.ssh/testkit
    install_method: pgdg-apt | pgdg-yum | distro
    cleanup: always
```

The testkit installs the agent, runs the scenario, collects logs, tears down (or `--keep` for forensics).

### The OS × PG × FS × Patroni × arch matrix

Maintained as `test/matrix.yaml` and expanded by `testkit matrix expand`:

```yaml
matrix:
  os:
    - { name: ubuntu-22.04, family: debian, arches: [amd64, arm64], packages: pgdg-apt }
    - { name: ubuntu-24.04, family: debian, arches: [amd64, arm64], packages: pgdg-apt }
    - { name: debian-12,    family: debian, arches: [amd64, arm64], packages: pgdg-apt }
    - { name: debian-13,    family: debian, arches: [amd64, arm64], packages: distro }
    - { name: rocky-9,      family: rhel,   arches: [amd64, arm64], packages: pgdg-yum }
    - { name: alma-9,       family: rhel,   arches: [amd64],        packages: pgdg-yum }
    - { name: rhel-9,       family: rhel,   arches: [amd64],        packages: pgdg-yum }
    - { name: fedora-40,    family: rhel,   arches: [amd64, arm64], packages: distro }
    - { name: opensuse-15,  family: suse,   arches: [amd64],        packages: distro }
    - { name: amazon-2023,  family: rhel,   arches: [amd64, arm64], packages: pgdg-yum }
  pg: ["15", "16", "17", "18-dev"]      # 18-dev is allowed-to-fail
  filesystem: [ext4, xfs, zfs, btrfs]   # zfs/btrfs only where kernel modules available
  patroni: [none, "3.x"]
```

Total combinations: ~640. We do **not** run all of them on every PR. Tiers:

| Tier | Scope | Wallclock | Trigger |
| --- | --- | --- | --- |
| **L1 smoke** | 1 cell: ubuntu-22.04 / pg-17 / ext4 / no-patroni / amd64 | ~5 min | every push |
| **L2 representative** | ~10 cells: one per OS family × one per PG major × one ARM | ~30 min | every PR |
| **L3 nightly** | ~80 cells: full OS × PG × ext4/xfs, no chaos | ~4 h | nightly cron on self-hosted CI |
| **L4 weekly** | full matrix, all FS, with chaos injection | overnight Sunday | weekly cron |
| **L5 pre-release** | L4 + 24 h soak on synthetic 1 TB dataset on real cloud VMs + differential vs pgBackRest/WAL-G | ~24 h | release candidate |

### Deterministic load generator

The hardest and most differentiating piece. Without this, restore-verification asserts "the database is restorable" but not "the database is *correct* at the LSN we asked for."

A workload is a `*.load.yaml` file. The generator is a single-purpose process that:
- Drives PG with a configurable mix of operations.
- Uses chacha20 PRNG seeded by the scenario seed → bit-for-bit reproducibility.
- Records a **checkpoint NDJSON stream** at every notable moment (phase boundary, every N seconds, every M MB of WAL produced) capturing expected state.
- Emits Prometheus metrics so test runs are observable in real time.

```yaml
schema: pg_hardstorage.testload.v1
seed: 0xC0FFEEBEAD42
locale: en_US.UTF-8
timezone: UTC

phases:
  - name: bootstrap
    duration: 5m
    operations:
      - create_table:    { name: users, schema: users_v1 }
      - insert_rows:     { table: users, count: 1000000, generator: faker_users }
      - create_table:    { name: orders, refers_to: users }
      - create_index:    { table: users,  columns: [email], unique: true }
      - create_index:    { table: orders, columns: [user_id, created_at] }

  - name: oltp_steady
    duration: 30m
    target_qps: 1000
    parallelism: 16
    mix:
      - { op: insert_orders,     weight: 50 }
      - { op: update_user_stats, weight: 30 }
      - { op: select_user_orders, weight: 15 }
      - { op: delete_old_orders, weight: 5 }

  - name: schema_evolution
    duration: 10m
    target_qps: 500
    operations:
      - alter_table:  { table: users, add_column: { name: phone, type: text } }
      - vacuum_full:  { table: orders }
      - reindex:      { table: users }

  - name: bulk_writes
    duration: 5m
    operations:
      - copy_in: { table: orders, rows: 5000000, generator: faker_orders }

checkpoints:
  every: 30s                                   # cadence checkpoint
  on:                                          # event-triggered checkpoints
    - { phase_end: bootstrap }
    - { phase_end: oltp_steady }
    - { phase_end: schema_evolution }
    - { phase_end: bulk_writes }
    - { lsn_advance: 1GB }                     # every 1 GB of WAL
    - { wallclock: "+22:00 UTC" }              # the exact moment for the user's example

  asserts_per_checkpoint:
    - count: { table: users }
    - count: { table: orders }
    - lsn:   pg_current_wal_lsn
    - digest: { table: users,  columns: [id, email, phone], algo: blake3 }
    - digest: { table: orders, columns: [id, user_id, created_at, amount], algo: blake3 }
    - page_aware_hash: { tables: [users, orders] }
    - pg_amcheck: { all: true }
    - sizes: { tables: [users, orders] }
    - schema_fingerprint: pg_dump_schema_only_blake3

# Optional: target-state snapshots that any later restore-verify must match
target_states:
  "22:00:00 UTC":
    users:  { count_exact: 1000000 }
    orders: { count_min: 800000, count_max: 900000 }
    digest: { table: users, columns: [id, email], blake3: "abc123def456..." }
```

Operations are pluggable: each `op` is a small Go function in `internal/testkit/load/ops/`. Generators (`faker_users`, `faker_orders`, etc.) are also pluggable; they consume the deterministic PRNG so results are repeatable.

The generator emits a sidecar file `<scenario>.checkpoints.ndjson` that is the ground-truth record of what existed at each LSN/wallclock. Restore-verify compares against this file.

For your concrete example: `pg_hardstorage testkit load checkpoint show --at "22:00 UTC" --scenario oltp.yaml` returns:

```json
{
  "at": "2026-04-28T22:00:00Z",
  "lsn": "0/3F5A1B40",
  "tables": {
    "users":  { "count": 1000000, "digest_blake3": "abc123..." },
    "orders": { "count": 100000,  "digest_blake3": "def456..." }
  },
  "schema_fingerprint": "blake3:9a8f2c..."
}
```

A test then does:

```yaml
- restore: { deployment: db1, to: "2026-04-28T22:00:00Z", target: /tmp/restored }
- assert_matches_checkpoint: { source: oltp.checkpoints.ndjson, at: "22:00 UTC" }
```

The assertion engine spins up the restored cluster, runs the same digest computation, and compares.

### Assertion DSL

Composable, declarative, schema-typed:

```yaml
asserts:
  - count_exact: { table: users, value: 1000000 }
  - count_range: { table: orders, min: 800000, max: 900000 }
  - digest_match: { table: users, columns: [id, email], algo: blake3, expected: "abc..." }
  - page_aware_hash_match: { tables: [users, orders], expected_from_checkpoint: "22:00 UTC" }
  - pg_amcheck: { passes: true }
  - pg_verifybackup: { passes: true }
  - lsn_at_least: "0/3F5A1B40"
  - schema_fingerprint_match: { expected: "blake3:9a8f2c..." }
  - sql:
      query: "SELECT count(*) FROM orders WHERE created_at < '2026-04-28T22:00:00Z'"
      expected: { rows: [[ 100000 ]] }
  - prom_metric:
      name: pg_hardstorage_resilience_panic_total
      delta_max: 0
  - no_orphan_chunks: true
  - no_uncommitted_manifests: true
  - audit_chain_intact: true
```

Each assertion has a clear failure message that explains what was expected vs what was observed, with diffs for digests and SQL results.

### Scenario files

```yaml
schema: pg_hardstorage.scenario.v1
name: full-restore-after-failover-with-deterministic-load
tier: L3                       # which CI tier this belongs to
description: |
  Full backup, leader failover mid-load, second backup, agent kill -9 mid-backup,
  restore to a checkpoint, assert byte-equivalence.

topology:
  provider: kind
  cluster_name: testkit-{{ .RunID }}
  operator: cnpg
  pg_version: 17
  replicas: 3
  filesystem: ext4
  patroni: managed_by_operator

agents:
  - on: pod/cnpg-cluster-1-1
    version: HEAD
    config:
      wal_mode: stream
      tenants: [default]

load:
  file: scenarios/oltp_with_failover.load.yaml

steps:
  - take_backup:        { deployment: db1, type: full }
  - run_load:           { duration: 10m }
  - inject:             { kind: patroni_failover, target: leader }
  - assert:             { failover_handled: true, slot_recreated: true, gap_bytes_max: 100MB }
  - run_load:           { duration: 10m }
  - take_backup:        { deployment: db1, type: full }
  - inject:             { kind: agent_kill, signal: 9, mid_op: backup, at_progress: 50 }
  - assert:             { agent_recovers_within: 60s, no_orphan_chunks: true, no_committed_partial_manifest: true }
  - take_backup:        { deployment: db1, type: full }
  - restore:            { deployment: db1, to_checkpoint: "phase_end:oltp_steady_2", target: /tmp/r1 }
  - assert_matches_checkpoint: { source: oltp_with_failover.checkpoints.ndjson, at: "phase_end:oltp_steady_2" }
  - assert:             { pg_amcheck: passes, pg_verifybackup: passes, audit_chain_intact: true }

cleanup:
  on_success: tear_down
  on_failure: keep_for: 2h     # forensics window
```

### Failure injection — `testkit inject`

| Layer | Tool | Examples |
| --- | --- | --- |
| Network | `toxiproxy` middleware | latency, drops, partial partitions, S3 503-storms |
| Disk | `dmsetup error_zone` | EIO regions, full-disk simulation |
| Memory | cgroup squeeze | force OOM-kill of the agent worker |
| Process | direct signal | `kill -9` at exact backup-progress percentage |
| Time | `libfaketime` | timezone, DST, NTP-skew |
| PG | replication API | drop slot, kill backend, promote replica |
| Patroni | DCS write | force leader change, simulate split-brain (briefly) |
| K8s | client-go | pod evict, node drain, network-policy update |
| Storage | wrapped Storage plugin | injected `503`/`429` per-key per-time-window |
| KMS | wrapped KMS plugin | force unwrap latency, simulate key disabled |

Every injection is recorded in the test artifact so a post-mortem shows exactly which fault was active when an assertion fired.

### Differential testing

```
$ pg_hardstorage testkit differential run --scenario oltp.yaml \
      --against pgbackrest --against walg
```

Same load, same topology, three tools take backups in parallel. Each restores into its own target. The testkit then computes table-level digests across all three restored databases. They must match (modulo timestamps and the like — the assertion engine has a "byte-equivalent except metadata" mode). Catches our regressions and gives us reasoning about parity.

### CI architecture

- **L1 smoke** in GitHub Actions on the standard runner pool.
- **L2 representative** in GitHub Actions matrix using `setup-pg`, plus a lightweight `kind` job for K8s coverage.
- **L3 nightly** on a self-hosted Kubernetes test cluster with persistent runners and pre-warmed images.
- **L4 weekly** on the same cluster + dedicated SSH-inventory bare-metal hosts for ZFS / Btrfs / RHEL coverage.
- **L5 pre-release** orchestrated via Terraform on real cloud accounts (AWS + GCP + Azure each get a representative slice) for the highest realism.

Test artifacts are uploaded as signed bundles (the same evidence-bundle format the LLM helper uses) so historical results are tamper-evident and replayable.

A real-time dashboard (Grafana, fed by the testkit pushgateway) shows pass/fail per matrix cell with trend lines; regressions Slack-ping with the failing scenario file and reproducer command.

### Reproducibility from a bug report

```
$ pg_hardstorage testkit reproduce --bug-report bug-1234.scenario.yaml
```

A user (or our LLM helper, after grounding from a customer's logs) emits a scenario file from a real failure. Anyone with `testkit` can reproduce locally. We require a bug report to include a runnable scenario before we accept it as a regression test target — this turns "weird intermittent thing in prod" into "here is a green-then-red commit set" within hours.

### Cutting-edge add-ons

- **Property-based scenarios.** `pgregory.net/rapid` generates random scenarios within a typed bounding box; the property is "for any (PG, OS, FS), backup → restore → digest_match holds." Surfaces edge cases hand-written scenarios miss.
- **Mutation testing.** Build-tagged fault injection in our own code (e.g., `//go:build mutation_chunker_off_by_one`) — re-run scenarios; assertions must catch the mutation. If they don't, we have a coverage gap.
- **Bisect mode.** `testkit scenario bisect --bad HEAD --good v0.1.0 --scenario X` walks commits to find regressors. Drops into git automatically.
- **AI-assisted triage.** When a test fails, the LLM helper (skill: `triage`) reads the test artifact and proposes a hypothesis. Useful for the long tail of weird failures; never auto-fixes — just suggests.
- **Coverage view.** `testkit coverage report --by code-path` correlates code paths to scenarios that exercise them. Shows where to add scenarios.
- **Multi-kernel via Firecracker.** Spin up microVMs with different kernel versions (5.15, 6.1, 6.6, 6.10) for kernel-fsync / io_uring behavior validation.
- **Hardware variance.** L4 cloud-VM tier deliberately mixes instance types (small/large, x86/arm, NVMe vs EBS-gp3) so we don't accidentally encode a "works on m7g.large only" assumption.
- **24-month manifest compatibility tests.** A snapshot of v0.1's repo + manifests is committed; every release runs `restore` against it and must succeed. Same for the JSON schema and the audit-chain format.

### What we still keep from the prior testing principles

The earlier list still applies — it's now Lvl-1/L2 of the tiered model:

- Unit (`go test`, ≥80% on hot packages, property-based on manifest, fuzz on parsers + chunker + natural-time + retention engine).
- Integration via `testcontainers-go` (PG 15/16/17, MinIO/Azurite/fake-gcs-server, localstack, vault-dev).
- `pg_verifybackup` parity gate on every CI backup.
- 3am simulation tests (operator-persona scripted recovery from synthetic failures).
- Restore drills (nightly).
- Performance regression benchmarks (>10% throughput / dedup / RTO regression fails CI).
- Race detector + static analysis on every test run.
- SLSA Level 3 build provenance on artifacts.

The testkit subsumes and extends all of these into a coherent, scenario-driven framework rather than ad-hoc scripts.

---

## Roadmap

The codebase is ahead of the original v0.1/v0.5/v1.0 targets. This section
reflects what is actually implemented today vs what remains.

### Implemented (current)

- Multiple binaries: `pg_hardstorage` (CLI + agent + minimal embedded control plane),
  `pg_hardstorage_testkit` (test harness), `pg_hardstorage_simple` (interactive
  menu), plus BusyBox-style `pg-hardstorage-compat` dispatcher serving
  pgBackRest / Barman / WAL-G / barman-cloud CLI shims.
- Linux systemd unit + goreleaser `.deb`/`.rpm`. Distroless container +
  `charts/pg-hardstorage-sidecar` for K8s.
- Backup: streaming `BASE_BACKUP`. Chunker: FastCDC with page-aware splitting.
  Repo: CAS, GC, init, wipe, worm, replicate, heal.
- Storage: `fs`, `s3`, `gcs`, `azblob`, `sftp`, `scp`.
- WAL: replication-protocol streaming (single-stream + replica-offload modes)
  + `archive_command` shim + `archive_library` Unix-socket endpoint.
  Patroni leader-following with permanent_slots (Strategy A), slot recreation
  (Strategy C), timeline history capture. Dual-stream, sync-target, and
  cascading modes planned.
- Compression: zstd. Encryption: AES-256-GCM with passphrase and full KMS
  envelope (AWS KMS, GCP KMS, Azure Key Vault, Vault Transit, PKCS#11/HSM).
- Per-tenant KEK + tenant-scoped RBAC. Full backup + PITR + pg_verifybackup
  gate + auto-rotate (GFS/simple/count/regulatory) + auto-fast-verify.
- REST API + gRPC + cobra CLI + `init` wizard + `doctor` + `status` +
  interactive `restore`.
- mTLS + token auth. FIPS build variant (`GOEXPERIMENT=boringcrypto`).
- Prometheus metrics, structured JSON logs, Merkle-hash-chained audit log.
- Coordination: JSON state files (single-host) + PG advisory locks (small
  fleet) + K8s Leases (in-cluster). No etcd dependency, no embedded SQLite.
- Sinks: `slack`, `webhook`, `syslog`, `pagerduty`, `email`, `cef`,
  `splunk-hec`, `datadog`, `jira`, `opsgenie`, `servicenow`, `teams`,
  `otelevents`, `discord`.
- Renderers: `text`, `json`, `ndjson`, `yaml`, `template`, `csv`, `html`,
  `markdown`, `pdf`, `tap`, `junit`.
- Typed `Event` bus + RFC 5424 severity levels + Renderer/Sink plugin tiers.
  Versioned `pg_hardstorage.v1` schema; NDJSON streaming; stable exit codes (0–10).
- Logical decoding stream (output plugins: pgoutput, wal2json, pg_hardstorage_proto;
  sinks: chunked, Kafka, Pub/Sub, webhook, S3 events; PII redaction transform).
- Partial / table-level restore. Hot-standby restore. Time-travel queries.
- Restore runbook generator. Restore checkpoints + atomic target switch.
- Verifier subsystem (Docker sandbox, pg_verifybackup + pg_amcheck + smoke SQL).
- Periodic scrub + auto-heal from replica region. Multi-source restore.
- Fleet view, anomaly detection, fleet-wide search.
- In-database SQL views (`CREATE EXTENSION pg_hardstorage`).
- n-of-m approvals, legal hold, data residency pinning, data classification tags.
- SLO-as-code, capacity & cost reporting, compliance report generator.
- SCIM 2.0, JIT access, insider-threat anomaly detection.
- Threshold k-of-n signing. Backup integrity continuous attestation.
- Crypto-shred API. WORM (S3 Object Lock etc.).
- Game-day automation (opt-in). Disaster runbooks R1–R7.
- Air-gapped bundle export/import. i18n (DE/FR/JA).
- LLM helper: `pg_hardstorage llm` TUI + MCP-server mode; providers `openai`,
  `mock`; skills as versioned YAML files (hot-reloadable, cosign-signed,
  linted+golden-tested in CI); skills `ask`/`explain`/`restore`/`incident`/
  `runbook`/`postmortem`; `--on-error-llm` auto-launch; privacy modes;
  mandatory `preview_command` before suggested mutations, replay-protected
  `execute_command`; Merkle-hash-chained audit; signed exportable evidence
  bundles.
- Testkit: `pg_hardstorage_testkit` binary; topology providers `local-docker`,
  `testcontainers`, `kind`, `ssh-inventory`; deterministic load engine with
  checkpoint NDJSON emitter; assertion DSL; scenarios as YAML; L1-L5 tiered CI;
  failure injection at every layer; differential testing vs pgBackRest/WAL-G;
  mutation testing; bisect mode; coverage report.

### Planned (not yet implemented)

- **`internal/supervisor/`** — parent-child self-supervision process model.
  The package exists as a scaffold but has no implementation. Currently systemd
  `Restart=always` provides process supervision.
- **`internal/repair/`** — unified repair toolkit (8 subcommands: manifest,
  chunks, wal, slot, index, attestation, scrub). The package exists as a
  scaffold. Individual repair paths (wal repair, slot repair) are handled
  inline in their respective packages.
- Additional LLM providers (bedrock, vertex, ollama, llama-cpp, huggingface).
- SAML 2.0 SSO + LDAP/AD integration.
- TDE awareness. pgaudit integration.
- Dual-stream, sync-target, and cascading WAL modes.
- Egress shaping per repo per time-of-day.
- Cross-account / cross-org repo replication.
- Status page / customer notifications.
- Tier-2 plugin protocol stable; public registry.
- Firecracker microVM verifier sandbox.
- Distro-native packaging (lintian-clean Debian source package + Fedora/RHEL spec).
- Community skill registry + public scenarios registry.
- 24-month manifest backward-compat commitment.
- SLSA Level 3 build provenance.

---

## Risks & open issues

1. **Chunk-store GC at scale** — LIST throttling on object stores. Mitigation: authoritative chunk-reference index alongside repo, transactional on commit.
2. **Compaction** — small chunks waste S3 per-object cost. Minimum 4 KiB size per chunk. Pack-file compaction deferred.
3. **Manifest schema evolution** — explicit migration framework; 24-month backward read commitment.
4. **WAL gap detection** — replication slot prevents stream gaps, but a long-disconnected agent + tight `max_slot_wal_keep_size` could lose WAL. Mitigation: paranoid auditor + alert before the line.
5. **Big-DB verifier compute** — hours to fully verify a 100 TB backup. Default sampled verification at this size; opt-in to weekly full.
6. **Patroni split-brain mid-backup** — `/leader` watcher + DCS lease prevents two committers; manifest commits only once.
7. **Encryption ↔ dedup tension** — cross-tenant dedup needs shared DEK pool, which most tenants reject. Default per-tenant DEK (no cross-tenant dedup); opt-in shared-pool mode.
8. **archive_library ABI churn** — young ABI (PG 15). Per-major library builds.
9. **CDC fingerprinting attack** — chunk-size leaks coarse byte-pattern info. Mitigation: per-tenant FastCDC salt.
10. **Natural-language time parser misinterpretation** — restore preview is mandatory before execution. `--confirm` required for actual mutation.
11. **Per-tenant KMS at 1000-tenant scale** — AWS KMS grants are per-key. Open: single-key + encryption context vs key-per-tenant trade-off.
12. **Logical decoding caveats** — DDL not captured by default (improving in PG 18); slot is primary-only (changing in newer PG); high primary CPU cost. We refuse to mark a deployment "backed up" on logical-only.
13. **Merkle audit-chain anchoring cadence** — per-event hashing is cheap; transparency-log anchoring needs throttling to avoid runaway Rekor costs. Default: hourly anchor.
14. **HSM availability** — PKCS#11 path adds a hard external dependency. Mitigation: HSM is opt-in; default is cloud KMS or local AES-GCM.
15. **PG advisory lock TTL** — PG advisory locks are session-bound, not TTL-bound. Coordination layer wraps them in a heartbeat goroutine; agent crash releases the session and the lock is auto-released. Document the implication: advisory locks survive ungraceful network partitions only as long as the TCP keepalive does.

---

## Verification (how we'll know the design works once built)

- **3am sim**: scripted "tired operator" recovery from a synthetic failure within target RTO, using only `pg_hardstorage doctor` + suggested commands. Pass = recovery without docs.
- **Big-DB soak**: 1 TB synthetic dataset, full backup → kill -9 mid-backup → resume → restore → `pg_amcheck` clean. (1 TB stands in for 100+ TB in CI; production validation on real customer data.)
- **Operator demo**: `kind` + CloudNativePG + our CNPG-I provider; `kubectl apply -f hsbackup.yaml` lands a backup in MinIO; `kubectl apply -f hsrestore.yaml` restores it.
- **Compliance demo**: WORM-locked bucket + per-tenant KEK; `pg_hardstorage kms shred --tenant T` makes T's backups unrecoverable; cosign signature on attestation; audit log entry written and signed.
- **Verifier demo**: scheduled job restores yesterday's backup in a Docker sandbox, runs `pg_amcheck` and a `SELECT count(*)`, posts `verification.json` back.
- **End-to-end demo on bare-metal**: `scripts/devcluster.sh` brings up a 3-node Patroni + MinIO + agents; `pg_hardstorage init`; `pg_hardstorage backup` + `pg_hardstorage restore --to "5 minutes ago"`; both pass.
- **Managed-DB demo**: backup an AWS RDS instance over the replication protocol (no host access). Demonstrates "WAL via DB connection, not URL" works in practice.

---

## Critical files (current implementation)

- [cmd/pg_hardstorage/main.go](cmd/pg_hardstorage/main.go) — main binary entry point (3-line shim)
- [cmd/pg_hardstorage_testkit/main.go](cmd/pg_hardstorage_testkit/main.go) — test infrastructure binary
- [cmd/pg_hardstorage_simple/main.go](cmd/pg_hardstorage_simple/main.go) — interactive quick-start helper
- [cmd/pg-hardstorage-compat/main.go](cmd/pg-hardstorage-compat/main.go) — multi-call compat dispatcher (pgBackRest/Barman/WAL-G/barman-cloud shims)
- [internal/cli/root.go](internal/cli/root.go) — cobra command tree root
- [internal/cli/init.go](internal/cli/init.go) — interactive setup wizard
- [internal/cli/restore.go](internal/cli/restore.go) — interactive + preview restore
- [internal/cli/doctor.go](internal/cli/doctor.go) — self-diagnosis
- [internal/agent/agent.go](internal/agent/agent.go) — long-lived agent process
- [internal/backup/orchestrator.go](internal/backup/orchestrator.go)
- [internal/backup/chunker/fastcdc.go](internal/backup/chunker/fastcdc.go) — FastCDC content-defined chunking
- [internal/backup/manifest.go](internal/backup/manifest.go) — backup manifest + signing
- [internal/backup/retention/gfs.go](internal/backup/retention/gfs.go) — GFS retention policy
- [internal/backup/keystore/keystore.go](internal/backup/keystore/keystore.go) — envelope encryption
- [internal/restore/orchestrator.go](internal/restore/orchestrator.go)
- [internal/restore/naturaltime/parse.go](internal/restore/naturaltime/parse.go) — "5 minutes ago" parser
- [internal/restore/checkpoint.go](internal/restore/checkpoint.go) — resumable restore state
- [internal/repo/cas.go](internal/repo/cas.go) — content-addressed store
- [internal/repo/layout.go](internal/repo/layout.go) — repository on-disk format
- [internal/repo/scrub.go](internal/repo/scrub.go) — bit-rot detector + auto-heal
- [internal/wal/stream/](internal/wal/stream/receiver.go) — physical WAL streaming (primary data plane)
- [internal/wal/audit/gap.go](internal/wal/audit/gap.go) — WAL gap detector
- [internal/wal/follower/coordinator.go](internal/wal/follower/coordinator.go) — Patroni leader-following
- [internal/wal/timeline/timeline.go](internal/wal/timeline/timeline.go) — timeline history storage
- [internal/coord/pgadvisory/coord.go](internal/coord/pgadvisory/coord.go) — small-fleet coordination (no etcd)
- [internal/coord/kubelease/coord.go](internal/coord/kubelease/coord.go) — K8s native coordination
- [internal/server/routes.go](internal/server/routes.go) — REST API routes
- [internal/server/server.go](internal/server/server.go) — control plane runtime
- [internal/plugin/storage/s3/s3.go](internal/plugin/storage/s3/s3.go)
- [internal/plugin/storage/fs/fs.go](internal/plugin/storage/fs/fs.go)
- [internal/plugin/storage/gcs/gcs.go](internal/plugin/storage/gcs/gcs.go)
- [internal/plugin/storage/azblob/azblob.go](internal/plugin/storage/azblob/azblob.go)
- [internal/plugin/storage/sftp/sftp.go](internal/plugin/storage/sftp/sftp.go)
- [internal/plugin/kms/awskms/awskms.go](internal/plugin/kms/awskms/awskms.go) — AWS KMS envelope encryption
- [internal/plugin/kms/gcpkms/](internal/plugin/kms/gcpkms/) — GCP KMS
- [internal/plugin/kms/azurekv/](internal/plugin/kms/azurekv/) — Azure Key Vault
- [internal/plugin/kms/vaulttransit/](internal/plugin/kms/vaulttransit/) — HashiCorp Vault Transit
- [internal/plugin/kms/pkcs11/](internal/plugin/kms/pkcs11/) — HSM / PKCS#11
- [internal/plugin/compression/zstd/](internal/plugin/compression/zstd/)
- [internal/plugin/encryption/aesgcm/](internal/plugin/encryption/aesgcm/)
- [internal/plugin/llmprovider/openai.go](internal/plugin/llmprovider/openai.go)
- [internal/output/event.go](internal/output/event.go) — typed Event, Severity, Subject, Suggestion
- [internal/output/dispatcher.go](internal/output/dispatcher.go) — fan-out to active Renderer + configured Sinks
- [internal/plugin/renderer/text/text.go](internal/plugin/renderer/text/text.go) — default renderer
- [internal/plugin/renderer/json/json.go](internal/plugin/renderer/json/json.go)
- [internal/plugin/renderer/ndjson/ndjson.go](internal/plugin/renderer/ndjson/ndjson.go) — streaming renderer
- [internal/plugin/sink/slack/slack.go](internal/plugin/sink/slack/slack.go)
- [internal/plugin/sink/webhook/webhook.go](internal/plugin/sink/webhook/webhook.go)
- [internal/plugin/sink/syslog/syslog.go](internal/plugin/sink/syslog/syslog.go)
- [internal/doctor/checks.go](internal/doctor/checks.go)
- [internal/schedule/scheduler.go](internal/schedule/scheduler.go)
- [internal/tenant/tenant.go](internal/tenant/tenant.go)
- [internal/runbook/generator.go](internal/runbook/generator.go) — 3am operator runbook generator
- [internal/gameday/scenarios.go](internal/gameday/scenarios.go) — opt-in chaos automation
- [internal/obs/metrics/](internal/obs/metrics/) — dependency-free Prometheus registry + `/metrics` exposition
- [internal/obs/resilience/metrics.go](internal/obs/resilience/metrics.go)
- [internal/fips/fips.go](internal/fips/fips.go) — BoringCrypto FIPS build variant
- [internal/audit/](internal/audit/) — Merkle-hash-chained audit log
- [internal/llm/](internal/llm/) — LLM assistant subsystem (chat, tools, safety, privacy, MCP, skills, evidence)
- [api/openapi.yaml](api/openapi.yaml) — OpenAPI 3.1 REST API specification
- [proto/](proto/) — protobuf definitions (gRPC)
- [ext/pg_hardstorage_archive/](ext/pg_hardstorage_archive/) — C archive_library extension

Planned but not yet implemented (package exists as empty scaffold):
- [internal/supervisor/](internal/supervisor/) — parent watchdog for agent worker
- [internal/repair/](internal/repair/) — unified repair toolkit
