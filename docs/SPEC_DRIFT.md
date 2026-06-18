---
title: SPEC ↔ code drift — engineering follow-ups
description: Items where SPEC.md describes behaviour the v0.9+ code doesn't yet implement.
---

# SPEC ↔ code drift — engineering follow-ups

Surfaced during the parallel-writer documentation sprint
(commit landing the Phase 2-7 content).  Each item is
either a missing implementation or a documentation
disagreement — **not** a doc bug.  These are tracked here so
they don't get lost; resolution is engineering work, not
doc work.

**v28 audit cycle (2026-05-05):** 9 of 15 items closed —
all 4 SPEC tightenings, all 3 godoc clarifications, plus
items #1 and #6 resolved by tightening SPEC + adding code
cross-references rather than reshaping the implementation.
Empty stub directories (`internal/crypto/envelope/`,
`internal/crypto/fips/`, `internal/plugin/source/`) removed
on the same pass.  6 items remain — all real engineering
work, deferred to v29.

## Open (v29 candidates)

| # | Area | Drift |
| --- | --- | --- |
| 2 | Crypto cipher | SPEC says **AES-256-GCM-SIV** is the default cipher; `internal/plugin/encryption/aesgcm/` only implements plain AES-256-GCM.  Either implement RFC 8452 or tighten SPEC. |
| 5 | Source plugin | SPEC promises a `SourcePlugin` interface; no Go code exists (the empty `internal/plugin/source/` directory has been removed; the SPEC promise stands).  Forward-looking shape documented in `docs/reference/plugins/source-contract.md`. |
| 10 | KMS rotate | `kms rotate` walks deferred to v0.5 per code comments; SPEC describes it as v0.1+ default. |
| 11 | Verify-full encrypted | `internal/cli/verify_full.go` has an unwired KEK resolver — `verify --full` of encrypted backups silently skips the KMS-decrypt path.  Documented as a v0.1 limitation; proper fix is wiring the resolver. |
| 12 | K8s integrations | **Partially closed in v1.1.**  pgBackRest, Barman, and WAL-G shims now ship as standalone binaries (`pg-hardstorage-{pgbackrest,barman,walg}`) — architecture changed from the SPEC's `agent --*-shim` flag form to segregated drop-in binaries (deliberate; see [compat/README.md](https://github.com/cybertec-postgresql/pg_hardstorage/tree/main/compat)).  CNPG-I provider remains genuinely deferred — still v0.5 work. |
| 4 (revised) | Crypto package layout | SPEC describes detailed envelope behaviour but the implementation lives in `internal/plugin/encryption/`.  The empty `internal/crypto/envelope/` and `internal/crypto/fips/` stub directories have been removed; no code change needed but the **product call** remains: do we want a top-level `internal/crypto/` namespace eventually, or is `internal/plugin/encryption/` the right home permanently? |

## Closed in v28

| # | Area | Resolution |
| --- | --- | --- |
| 1 | Patroni CLI | Tightened SPEC.  `--patroni-url` was a SPEC narrative artefact; the implementation correctly uses per-deployment YAML because the agent processes many deployments per host.  SPEC.md edited to reflect the YAML form; CLI tree note in SPEC's CLI surface clarified. |
| 3 | Audit anchoring | Tightened SPEC.  Hash-chain ships in v0.5; Rekor anchoring lands in v1.0 (matches `internal/audit/audit.go`). |
| 6 | Tier-2 protocol | Added v1.0 vs v1.x cross-reference at the top of `internal/plugin/external/protocol.go`.  v1.0 wire contract is stdio JSON-RPC (shipped); proto file is the v1.x target.  No code change. |
| 7 | Metrics | **Implemented.**  `internal/obs/metrics/` now ships a dependency-free Prometheus registry; `/metrics` is served by the control plane (always, unauthenticated) and by the agent (opt-in via `agent --metrics-listen`).  The backup, restore, replicate, WAL-archive, verify, KMS, chunk-upload, control-plane-HTTP, control-plane-error, job, agent, and build-info families are live and scrape real data; the SLO / anomaly / resilience / repo-size / WAL-lag families remain reserved names (see the metric catalogue's "Reserved" section and drift #8).  doctor + audit log remain the canonical surfaces for the not-yet-live signals. |
| 8 | SLO metric | Same fix as #7. |
| 9 | Cost reporting | Tightened SPEC.  Per-deployment cost ships in v0.5; per-tenant aggregation (`--by-tenant`) is a v1.x polish item. |
| 13 | Compression | godoc note added to `CodecRegistry` clarifying production callers wire codecs **per-CAS**, not at process-global init. |
| 14 | Renderer registry | godoc note added to `Renderer` interface explaining why no `DefaultRegistry` exists (one-renderer-per-invocation; lookup ceremony unwarranted; in-tree extension is the right posture). |
| 15 | KMS interface | godoc note added to `kms.Provider` clarifying that DEK generation lives in `internal/backup/keystore` and KEK rotation lives in `internal/cli/kms_rotate.go`; plugin authors implement only Wrap/Unwrap/Shred. |

## Recommended resolution

The 6 remaining items split as:

- **Implement** — match the code to the SPEC (3 items: #2,
  #5, #11)
- **Decide and document** — needs a product call (3 items:
  #4-revised, #10, #12)

Total estimated effort to close the v29 backlog: ~1 week of
focused engineering plus 3 product decisions.

The doc project is **not blocked** by these items.  The docs
currently describe what's actually shipped, with
forward-looking notes on items where the gap is most
visible.
