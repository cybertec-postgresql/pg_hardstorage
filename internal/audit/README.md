# audit/

Hash-chained, tamper-evident audit log of every operator-visible action against
the repo.

## What lives here

Each `Event` records one action (backup committed, hold placed, KMS rotation,
JIT issued, approval signed). Events are appended in-order; every event carries
the HMAC hash of the previous event, so any tampering breaks the chain. Periodic
*anchors* publish the chain head to an external transparency log
(Rekor-compatible) so a compromise of the local repo is detectable from outside.

### Sharded chains

The log is partitioned into independent hash chains — *shards* — keyed by the
most specific scope an event carries: deployment, then tenant, then a **global**
chain for repo-level events. Each shard is its own tamper-evident chain (own head
pointer, own sequence, own `PrevHash` linkage), so appends to different scopes
never contend on one shared head pointer — the serialization point that made a
single per-repo chain a bottleneck at fleet scale.

- The shard key is derived from the event by `shardKeyFor`, the only
  dimension-dependent piece. An event's scope is part of its canonical hash, so
  it can't be moved between shards without breaking its hash; `VerifyChain` also
  flags any event filed under a shard its scope doesn't imply (`Misfiled`).
- Layout: the **global** shard keeps the legacy paths (`audit/<yyyy>/...`,
  `audit/_head.json`) for backward compatibility — existing repos stay valid with
  no migration. Named shards live under `audit/shards/<shard>/...`.
- `VerifyChain` and `Search` iterate every shard and aggregate; transparency
  `Anchor` currently witnesses the global chain (per-shard anchoring is a
  follow-up — each shard remains independently verifiable meanwhile).

## Key files

- `audit.go` — `Event`, `Append`, `Walk`, `Search` (filter by actor / kind /
  time)
- `computehash.go` — canonical event hashing (the chain primitive)
- `bundle.go` — export a signed evidence bundle for an auditor (tar.gz,
  optional encryption)
- `transparency.go` — anchor the chain head into Rekor / a transparency log
- `rand.go` — chain-internal nonce generation
- `head_pointer_test.go` / `worm_propagation_test.go` — coverage for the
  head-pointer + WORM-repo behaviour
- `*_mutation_*.go` — mutation-audit shims (proof that tampering is caught)

## Read next

- `../threshold/README.md` — multi-party blessings on anchors and audit-bundle
  exports
- `../approval/README.md` — emits audit events on every create + approve
- `../jit/README.md` — emits events on issue + use + revoke
- `../insider/README.md` — consumes audit events to score insider-threat risk
- `../README.md` — parent index

## Don't put X here

- Repo-side encryption or WORM enforcement — that's `internal/repo/`.
- KEK lifecycle — that's `internal/kms/` and `internal/plugin/kms/`.
