# internal/

Subsystem index. Every Go package under `internal/` is private to this module;
if you're searching for where a feature lives, start here.

## Read next

- `../cmd/README.md` if it exists — binary entrypoints wire these subsystems
  together
- `../proto/` — gRPC contracts (Go server unbuilt; see `server/`)
- `../docs/reference/` — user-facing reference for the surfaces below

## Core data path

- [`backup/`](backup/README.md) — chunker, manifest, runner, retention,
  keystore, tarsink, verifybackup, attestgate
- [`restore/`](restore/README.md) — plain + chain-restore via
  `pg_combinebackup`, preflight, postverify, naturaltime, PITR
- [`pg/`](pg/README.md) — Postgres wire layer: BASE_BACKUP, physical + logical
  replication, WAL sink, streaming reader
- [`cli/`](cli/README.md) — Cobra command tree (60+ subcommands), output
  dispatcher, arg validation, YAML config IO
- [`repo/`](repo/README.md) — content-addressed store: CAS, layout, GC,
  scrub+heal, replicate, WORM, ACL, bundle
- [`wal/`](wal/README.md) — WAL coordination: Patroni follower, gap state,
  archive inventory, timeline capture

## Operational

- [`agent/`](agent/README.md) — long-lived supervised process: controlplane,
  job executor, restore/verify dispatch
- [`server/`](server/README.md) — REST HTTP server: 10 routes, mTLS+auth,
  jobs/agents/heartbeat handlers
- `fleet/` — multi-deployment fleet view used by the agent controlplane
- `schedule/` — cron-style schedule planner consumed by the CLI and agent
- `obs/` — observability primitives (counters, traces) used across packages

## Crypto + compliance

- `audit/` — tamper-evident audit log (HMAC chain, transparency anchors,
  WORM propagation)
- `kms/` — abstract KMS interface; concrete adapters live in `plugin/kms/`
- `threshold/` — Shamir-style k-of-n quorum for KEK release
- `jit/` — just-in-time privilege grants with TTL
- `scim/` — SCIM 2.0 user/group provisioning
- `integrity/` — cross-cutting tamper checks (manifest + audit + repo
  hash chains)
- `approval/` — multi-party approval workflow (request → vote → execute)
- `fips/` — FIPS-140 build-tag gated crypto selection

## Enterprise

- `anomaly/` — behavioural anomaly detector over audit events
- `insider/` — insider-threat heuristics on operator actions
- `dsa/` — data-subject-access (GDPR Art. 15) request runner
- `compliance/` — controls catalog + report rendering (markdown, csv)
- `forecast/` — capacity forecasting from historical telemetry
- `capacity/` — live capacity probes and preflight gates
- `cost/` — cost projection per backup/storage tier
- `gameday/` — DR exercise scenarios (chaos drills)
- `recovery/` — recovery-drill runner, history, readiness, WAL inventory,
  windows
- `slo/` — SLO definitions and burn-rate tracking (skeleton)

## Deferred-surface (v0.5 / v1.0)

- `standby/` — standby bring-up from a backup; v0.5 surface, repo glue stubbed
- `timetravel/` — point-in-time browse of restored data; v0.5 surface
- `partial/` — single-relation / tablespace partial restore; v1.0 surface,
  sandbox harness present
- `logical/` — logical-replication runner + lag monitor; v0.5, sinks pluggable

## Supporting

- `llm/` — LLM-assisted chat, docs, history, MCP bridge, privacy scrubber,
  skills, tool dispatch
- `output/` — output dispatcher: event model, severity, sinkspec, renderer
  protocol, exit codes
- `config/` — YAML config loading + Patroni-validation
- `patroni/` — Patroni HTTP client + leader-follower watcher
- `paths/` — OS-aware path resolution (XDG, Windows)
- `verify/` — verification sandbox for backup attestation
- `plugin/` — pluggable contracts: compression, encryption, kms, llmprovider,
  renderer, sink, storage, external
- `testkit/` — test infrastructure: runner, scenario, load, inject, sink,
  topology, assert, validate, bisect, mutation, catalog
- `airgap/` — air-gap export/import bundle helpers
- `chain/` — backup-chain graph utilities (shared between backup + restore)
- `dbext/` — Postgres extension installer + introspection
- `fsutil/` — filesystem helpers (atomic rename, fsync, safe-join)
- `i18n/` — message catalog for localised CLI output
- `repoaudit/` — repo-side audit-bundle reader
- `regression/` — regression-suite harness wrapper
- `simple/` — `pg_hardstorage_simple` binary's command surface
- `version/` — build-time version + revision string
