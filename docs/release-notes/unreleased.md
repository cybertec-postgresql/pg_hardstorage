---
title: Unreleased
description: What's landed since v1.0 — fleet-scale improvements and correctness hardening.
---

# Unreleased

> Changes on `main` since v1.0, distilled for operators. The full
> commit-level detail is in the [`CHANGELOG`](../changelog.md).

The headline of this cycle is **scaling to large fleets**: a set of
changes that keep the control plane and repository fast and bounded
across thousands of deployments, plus a round of correctness and
WORM-compliance hardening. Everything here is **backward compatible**
— no on-disk migration, and the new fleet knobs are opt-in (defaults
preserve existing behaviour).

---

## Scaling to large fleets

Four bottlenecks that bite at thousands of deployments are gone. See
the new [Scaling to large fleets](../operations/scaling-large-fleets.md)
guide for the full picture and sizing guidance.

### Sharded audit chains

The tamper-evident audit log used to be a single hash chain per repo:
every append, across every deployment and tenant, serialized behind
one head pointer. It's now partitioned into independent per-scope
chains (deployment → tenant → global), so appends don't contend and a
concurrent append to one deployment can't fork another's. Integrity is
preserved and strengthened — `audit verify-chain` checks every shard
and flags any event filed under the wrong shard (`misfiled`). The
global chain keeps the legacy on-disk layout, so existing repos stay
valid with no migration. See
[Audit chain → Sharded chains](../explanation/audit-chain.md#sharded-chains).

### Fast deployment enumeration

Listing the fleet (`GET /v1/deployments` and the many internal
callers — retention, GC, capacity, status) used to scan every manifest
object. It now reads a small per-deployment index maintained at
commit time, turning an O(total backups) scan into O(number of
deployments). The index self-heals and backfills automatically; no
configuration needed.

### Agent poll jitter

Agents jitter their heartbeat/poll intervals (±20%, with the first
tick spread across the interval), so a fleet started — or restarted —
together no longer hits the control plane in synchronized bursts. On
by default; the mean request rate is unchanged.

### Job-concurrency cap

A new `--max-concurrent-jobs` (or `server.max_concurrent_jobs` in the
config) caps how many jobs run at once across the fleet, so a burst of
queued work can't storm storage and the source databases. Claims are
refused once the cap is hit and queued work waits for a slot. Zero
(the default) is unlimited; with the PostgreSQL coordination backend
the cap is enforced globally across control planes.

---

## Correctness and WORM hardening

### Concurrent backups of the same deployment are now prevented

A per-deployment **backup lease** stops two backups of the same
deployment — even from different agents that only share the repo —
from running at once (which previously doubled load on the source
primary). A second backup is refused with `conflict.backup_in_progress`;
a crashed holder's lease expires and is reclaimed automatically. Opt
out per run with `backup --allow-concurrent`.

### WORM retention now lands on the committed manifest

Manifest commit applied Object-Lock retention to the staging tmp
object rather than the committed manifest — which on a Compliance
bucket broke the commit outright, and on GCS/Azure applied no
retention at all. Retention is now applied to the committed manifest
(verified against a real MinIO Object-Lock bucket). If you rely on
WORM, the committed manifests are now correctly locked.

### GC no longer races in-flight backups; staging files are swept

`repo gc` gained a chunk-age floor so a sweep running alongside an
in-flight backup can't reap chunks the backup has written but not yet
referenced. It also now removes stale `*.json.tmp.*` staging files left
by interrupted commits (`--min-chunk-age` tunes the floor; `0`
disables).

### Other fixes

- **Audit bundle import** now actually enforces `ImportOptions.Verifier`
  — signed manifests are verified before ingest instead of being
  written unverified, and caps total entry count and bytes.
- **WAL gap-state purge** wipes corrupt records on an end-to-end purge
  and deletes by the exact stored key.
- **Untrusted-input hardening** — manifest and CAS chunk-envelope reads
  are size-bounded, deployment/backup IDs are validated at the
  manifest-store chokepoint, and agent heartbeat fields are bounded and
  sanitized, so a corrupt repo or hostile payload can't drive unbounded
  allocation.
- **Fail-loud error handling** — the job sweeper recovers panics and
  surfaces sweep errors, `repo gc` fails closed on an unparseable chunk
  reference, and manifest-rollback / logical-receiver flush failures are
  reported instead of dropped.
- **Bounded memory** — retained jobs / per-job progress and the CAS
  dedup `seen` cache are capped so a long-running process can't grow
  without limit.

---

## Diagnostics: `doctor` catches more, cries wolf less

### `doctor` now flags backups orphaned by a lost or rotated signing key

A lost, ephemeral, or regenerated manifest-signing keyring leaves every
backup signed with the *old* key — verification fails with
`ErrPublicKeyMismatch` and those backups can no longer be restored or
verified. Previously `doctor` still reported `healthy: true /
signing_key_exists: true` in exactly that situation, because it only
checked that *a* signing key existed, not that it matched the backups.

`doctor` now verifies each deployment's manifests against the current key
(oldest-first, bounded so it stays fast on large repos — and oldest-first
is where a lost key strands its backups) and emits
`manifest_signature_mismatch` (warning) when any fail, with a
`manifest_signatures[]` report section recording checked / mismatched
counts per deployment. The remediation points at restoring the original
key, or `repo check` for the exhaustive list. `list` got the same
treatment: on a key mismatch it now says the backups exist but failed
signature verification and points at `doctor` / `repo check`, instead of
the misleading "No backups for db1" that read as data loss.

### `audit.anchor_stale` no longer fires on a healthy chain

`doctor` judged the transparency-log anchor's freshness by **counting**
audit event files and comparing that count to the anchor's head sequence.
That denominator is wrong in two real configurations, each producing a
false `audit.anchor_stale` notice — with a nonsensical *negative*
"events behind" number that re-anchoring could never clear:

- **WORM retention** prunes the oldest events while sequence numbers keep
  climbing, so on an aged regulated repo the count falls below
  `head_sequence + 1`; and
- the audit log is **sharded**, so the count spans every shard while an
  anchor witnesses only one.

Freshness now compares the anchor against the authoritative head pointer
of the anchor's *own* shard — a perf cache retention deliberately leaves
alone — so a fresh anchor reads fresh regardless of repo age or sharding.

### CLI examples in the docs now match the binary

A cross-check of every `pg_hardstorage … --flag` shown in the prose docs
against the real command surface caught a batch of examples referring to
flags and verbs that don't exist (`doctor --fix`, `agent --log-level`,
`kms rotate --new-kek`, `audit append --type`, `kms shred --kek-ref` for a
cloud KEK, …). They've been corrected to runnable commands or the real
mechanism; the auto-generated CLI reference, manpages, and completions
were already in lockstep with the code.

---

## LLM helper: more correct, harder to drown

Empirical testing against a smaller model surfaced several ways the
operator assistant gave advice that failed when followed verbatim.
Fixed:

- **The command-validator was noisy and missed real errors.** It now
  reads commands only from fenced code blocks (not prose or comments,
  which produced spurious "unknown subcommand" warnings), understands
  that a runnable command like `backup <deployment>` takes a positional
  (so `backup db1` is no longer mis-flagged), and detects a **missing
  required flag** — `rotate db1 --apply` without `--repo`, `backup db1`
  without `--pg-connection`, etc.
- **Required flags are now declared, not just hand-checked.** Every command
  with an *unconditionally* required flag (`--repo`, `--pg-connection`,
  `--from`/`--to`, the KEK refs/files, `--scope`, `--url`, …) moved from manual
  RunE checks to cobra's `MarkFlagRequired`, so `--help`, shell completion, and
  the LLM command-validator all read one source of truth. Cobra's required-flag
  failure is translated back to the same `usage.missing_flag` error + exit code,
  so the contract is unchanged.
  Commands whose requirement is *conditional* — needed only in local mode
  (`backup`/`restore`/`verify` skip it when dispatching to a control plane),
  satisfiable by a positional URL instead of `--repo` (`repo audit/gc/check/
  usage`, `compliance report`, `capacity preflight`, `forecast`), or gated by
  another flag (`db install-extension` / `redact apply` waived by `--dry-run`,
  `logical remove --drop-slot`, `timetable emit --apply`) — use a small runtime
  guard helper (`requireFlags` / `missingFlagErr`) that produces the identical
  structured error. The only remaining hand-written checks are genuine *value*
  validations a presence flag can't express (`--threshold` ≥ 1, `--tables`
  non-empty after splitting, `--approver-key` count, `--query` whitespace).
- **Destructive commands described as harmless now self-correct.** A
  command carrying `--apply` / `--force` / `--yes` (or `shred` / `wipe`)
  that the surrounding text calls a "dry-run" or "safe" feeds the same
  retry loop as a structural error: the model is re-prompted to drop
  `--apply` (a real dry-run) or relabel it as the executing step. If it
  still won't, the warning surfaces — the operator is never told a delete
  is safe. (The dry-run is the same command *without* `--apply`.)
- **The incident skill no longer overflows the context window.**
  Pre-loaded `doctor` / `status` output (and tool results) are capped
  per call, so a large or broken repo can't push the prompt past the
  model's limit and fail the request. (The companion `list` fix —
  surfacing key-mismatch-orphaned backups instead of "No backups" — is
  under Diagnostics above.)
- **The validator checks positional-argument counts.** A hallucinated
  subcommand like `backup full db1` (there is no `full` — it's just
  `backup <deployment>`) used to slip through as a stray positional; it's
  now flagged (*"accepts 1 positional argument, got 2"*) and fed to the
  self-correction loop.
- **Always-on prompt rules** tell the model to include required flags,
  never mislabel a destructive command, never invent file paths, use real
  verbs with the right number of positional arguments, **check the
  pre-loaded `doctor` / `status` state before state-dependent advice**
  (no `kms rotate --old-kek-file` when no KEK exists), lead with the single
  highest-likelihood next action, and prefer the tool's own
  structured-error remediation.

---

## Observability

A Prometheus `/metrics` endpoint now serves real data — always-on and
unauthenticated on the control plane, opt-in on the agent via
`agent --metrics-listen`. See the
[metric catalogue](../reference/metric-catalogue.md) and
[monitoring guide](../operations/monitoring.md).

This cycle also widened what's actually metered. **Restore** is now
instrumented end-to-end (`restore_started_total`,
`restore_completed_total{result}`, `restore_duration_seconds`),
**`repo replicate`** emits progress events plus run / objects / bytes
counters so a long cross-region copy is no longer opaque, and
**control-plane agent** loop failures increment
`controlplane_errors_total{op}` so fleet-health degradation is alertable
rather than buried in stderr.

On the **audit** side, two operator actions that previously left no
tamper-evident record now do: placing or releasing a **legal hold**
(`hold add` / `hold remove`), and each backup a **retention run**
(`rotate --apply`) soft-deletes. A compliance reviewer can now
reconstruct both from the audit chain.

---

## Upgrading

- **No migration.** All on-disk layouts are backward compatible. The
  audit log's global chain keeps its existing paths; sharded chains and
  the deployment index are created lazily as new events / backups land.
- **New knobs are opt-in.** `--max-concurrent-jobs` defaults to
  unlimited and poll jitter is on by default with no configuration;
  the backup lease is on by default and is the only behavioural change
  you might notice (a second concurrent backup of the *same* deployment
  is now refused — pass `--allow-concurrent` if you intend it).
- **`doctor` may surface a new warning** if you have backups signed by a
  key your keyring no longer holds: `manifest_signature_mismatch`. It's a
  real finding (those backups can't be restored), and it trips
  `doctor --exit-on-issues` (exit 10), so a cron/liveness probe that was
  green could now alert — which is the point. Restore the original signing
  key, or take a fresh backup, to clear it.
- **No CLI contract change from the required-flag work.** Commands that
  moved to `MarkFlagRequired` still return the same `usage.missing_flag`
  error code and exit code on a missing flag; only the wording of cobra's
  message differs from the old hand-written strings.
- See the [`CHANGELOG`](../changelog.md) for the complete list.
