<!-- AUTO-GEN candidate: grep `output.NewError("…")` over the tree, dedupe, group by namespace; per docs/DOC_PLAN.md auto-generation map. -->
---
title: Error codes
description: Catalogue of structured error codes — grouped by domain, paired with exit code and recovery path.
tags:
  - reference
  - errors
  - cli
---

# Error codes

Every error a CLI command can surface is a structured
`*output.Error` with a dotted, lowercase `Code` (see
[`internal/output/event.go`](https://github.com/cybertec-postgresql/pg_hardstorage/blob/main/internal/output/event.go)).
The first dotted segment is the **namespace**; it drives
the [exit code](exit-codes.md) and clusters codes by
domain.

This page lists the namespaces shipped today, their stable
properties, and the typical recovery path.  Individual
leaf codes are not exhaustively listed — the surface is
~270 codes — but every leaf belongs to one of the
namespaces below, and every namespace honours the
properties documented here.

## Quick map: namespace → exit code → recovery

| Namespace | Exit | Recovery |
| --- | --- | --- |
| `usage.*` | 2 | Fix the invocation; re-run. |
| `auth.*` | 3 | Re-authenticate; check approval roster. |
| `preflight.*` | 4 | No mutation occurred. Fix the underlying condition; retry. |
| `aborted.*` | 5 | Operator-driven; safe to retry. |
| `notfound.*` | 6 | List the resource; pick a real ID. |
| `conflict.*` | 7 | Wait for the holder, or pick a fresh name. |
| `verify.*`, `anomaly.*` | 9 | Investigate; do not retry blindly. |
| `doctor.*` | 10 | Read the doctor body; address the issue. |
| `storage.unreachable`, `kms.unreachable` (leaf) | 8 | Transient by nature; retry with backoff. |
| every other namespace | 1 | Generic; consult the leaf code and the suggestion. |

`*output.Error.Suggestion` carries the human + command +
doc-URL remediation triple; the renderer surfaces it
unmodified.  When triaging, always read `suggestion.command`
first — that's the literal shell string the binary thinks
will fix the problem.

---

## `usage.*` — bad invocation (exit 2)

Wraps every CLI parse / validation refusal.  Cobra's own
"unknown flag" / "missing arg" errors are remapped to this
namespace so the exit code is uniform.

Common leaf shapes:

| Leaf | Meaning |
| --- | --- |
| `usage.bad_flag`, `usage.bad_flags` | Flag value parse failed |
| `usage.bad_arg`, `usage.bad_set` | Positional argument shape mismatch |
| `usage.missing_*` | Required input absent (`missing_repo_url`, `missing_deployment`, `missing_signer`, `missing_target_dir`, …) |
| `usage.bad_lsn`, `usage.bad_target_lsn`, `usage.unaligned_lsn` | LSN parse / alignment refused |
| `usage.bad_time`, `usage.bad_until`, `usage.bad_schedule` | Time / cron parse refused |
| `usage.bad_token`, `usage.bad_key_file`, `usage.bad_approver_key` | Cryptographic input did not parse |
| `usage.confirmation_required`, `usage.confirmation_mismatch` | Typed-keyring confirmation missing or wrong |
| `usage.conflicting_flags`, `usage.conflicting_targets` | Mutually-exclusive flags both set |
| `usage.unknown_output_format`, `usage.unknown_policy`, `usage.unknown_scheme`, `usage.unknown_shell` | Enum-shaped value out of set |

**Recovery:** read the suggestion, fix the command line,
re-run.

---

## `auth.*` — authentication / authorization (exit 3)

| Leaf | Meaning |
| --- | --- |
| `auth.denied` | RBAC refused the operation |
| `auth.key_mismatch` | Operator key does not match the configured roster |
| `auth.approver_not_allowed` | Approver in an n-of-m flow is not on the roster |
| `auth.approval_op_mismatch`, `auth.approval_target_mismatch` | Approval token bound to a different op or subject |

**Recovery:** rotate credentials, re-run with the right
operator identity, or update the approval roster.

---

## `preflight.*` — refused before any mutation (exit 4)

The contract: a `preflight.*` error means **no on-disk or
remote state changed**.  Safe to retry once the condition
is fixed.

| Leaf | Meaning |
| --- | --- |
| `preflight.repo_full` | Capacity preflight refused; not enough free space |
| `preflight.target_not_empty`, `preflight.target_not_dir`, `preflight.target_read`, `preflight.target_stat` | Restore target directory not in expected shape |
| `preflight.target_running_postgres`, `preflight.target_postmaster_unverifiable`, `preflight.target_pg_datadir`, `preflight.target_foreign_cluster` | Restore refused to overwrite the target: a running cluster (never overridable), an existing datadir (override with `--force`), or a DIFFERENT cluster by `pg_control` system-identifier mismatch (override with `--force-foreign`) |
| `preflight.tablespace_not_empty`, `preflight.tablespace_not_dir`, `preflight.tablespace_stat`, `preflight.tablespace_read` | A `--tablespace-mapping` target dir is in the wrong shape or non-empty; refused so a restore can't clobber another cluster's tablespace (override the non-empty case with `--force`, which clears it) |
| `preflight.pg_version_mismatch` | Target PG version does not match the backup's |
| `preflight.pg_combinebackup_missing`, `preflight.pg_tools_missing` | Required PG client tools not on `$PATH` |
| `preflight.checkpoint_check_failed` | Source PG refused to issue a checkpoint |
| `preflight.chain_target_not_empty` | Replicate-by-chain target already has content |

**Recovery:** read the body, fix the condition (free disk,
upgrade PG client tools, clear the target dir, …), retry.

---

## `aborted.*` — operator or context aborted (exit 5)

| Leaf | Meaning |
| --- | --- |
| `aborted.context_cancelled` | `ctx.Done()` fired (SIGINT, request deadline) |
| `aborted.confirmation_required` | Interactive confirmation refused at the prompt |
| `aborted.backup_cancelled`, `aborted.restore_cancelled`, `aborted.verify_cancelled` | Operator cancelled a long-running op |
| `aborted.attestation_valid`, `aborted.primary_intact` | Safety belt fired (e.g. `restore` against a still-healthy primary) |

**Recovery:** retry when the operator is ready.

---

## `notfound.*` — named resource not present (exit 6)

| Leaf | Meaning |
| --- | --- |
| `notfound.backup`, `notfound.backup_before_time`, `notfound.backup_tombstoned` | Backup ID / time-pin / liveness mismatch |
| `notfound.deployment`, `notfound.repo`, `notfound.sink` | Configuration entity missing |
| `notfound.wal_segment`, `notfound.wal_segment_name` | WAL not present in the repo |
| `notfound.session`, `notfound.token`, `notfound.skill`, `notfound.skill_snapshot` | LLM session / skill state |
| `notfound.replica_manifest`, `notfound.attestation` | Manifest / attestation absent |

**Recovery:** `pg_hardstorage list <kind>` to see what's
actually there.

---

## `conflict.*` — collision or holder present (exit 7)

| Leaf | Meaning |
| --- | --- |
| `conflict.repo_exists`, `conflict.deployment_exists`, `conflict.sink_exists`, `conflict.standby_exists`, `conflict.timetravel_exists`, `conflict.roster_exists` | Resource with that name is already configured |
| `conflict.repo_read_only` | Repo is in read-only mode (legal hold, scheduled retire) |
| `conflict.manifest_held`, `conflict.chain_has_held_links` | Backup or one of its parents is on legal hold |
| `conflict.chain_has_live_descendants` | Refused to delete; descendants would orphan |
| `conflict.checkpoint_mismatch`, `conflict.chunks_missing`, `conflict.no_live_manifests` | Repo state would be inconsistent |
| `conflict.approval_pending`, `conflict.already_signed`, `conflict.already_revoked` | Approval-flow state machine refused |
| `conflict.too_many_connections` | PG refused another replication / regular connection |

**Recovery:** wait, pick a different name, or release the
hold.

---

## `verify.*` — verification failed (exit 9)

The single most operationally consequential namespace —
treat any `verify.*` exit as an alert.

| Leaf | Meaning |
| --- | --- |
| `verify.checksum_mismatch`, `verify.chunk_size_mismatch`, `verify.short_assembly`, `verify.scrub_mismatch` | CAS chunk corruption |
| `verify.missing_chunks` | Manifest references a chunk not present in the repo |
| `verify.manifest_signature`, `verify.replica_signature`, `verify.dsa_signature`, `verify.integrity_signature` | Signature verification failed |
| `verify.attestation_invalid`, `verify.attestation_quorum`, `verify.attestation_roster`, `verify.attestation_subject` | Attestation refused |
| `verify.kek_mismatch`, `verify.kek_resolve_failed`, `verify.bad_wrapped_dek` | KEK / DEK decrypt failed |
| `verify.envelope_break` | Envelope-encryption tag did not validate |
| `verify.replica_inconsistent`, `verify.replica_identity_mismatch` | Cross-region replica disagrees with primary |
| `verify.audit_anchor_mismatch`, `verify.audit_chain_broken` | Audit hash chain is broken |
| `verify.residency_violation` | Data-residency policy refused the action |
| `verify.wal_gap_detected` | A Patroni-failover WAL gap covers the requested PITR window |
| `verify.heal_incomplete`, `verify.integrity_issues`, `verify.insider_findings` | Resilience checks surfaced findings |

**Recovery:** never auto-retry.  Treat as a P1 incident;
follow the relevant
[runbook](runbooks/index.md).

## `anomaly.*` — baseline-shift detected (exit 9)

Same exit code as `verify.*` so cron alarms uniformly,
but distinct semantics: the backup itself is fine; the
`anomaly check` heuristic flagged a baseline shift.

| Leaf | Meaning |
| --- | --- |
| `anomaly.detected` | Z-score / dedup-ratio / size delta exceeded threshold |
| `anomaly.score_failed` | Anomaly scorer itself errored |

---

## `doctor.*` — health-check finding (exit 10)

| Leaf | Meaning |
| --- | --- |
| `doctor.path_resolve` | Resource resolution refused; doctor reports the issue |

`doctor` only emits this exit code when invoked with
`--exit-on-issues`; otherwise findings are reported and
the binary exits 0.

---

## Operational namespaces (exit 1 by default)

These do not carry a special exit code.  Each is the place
where one subsystem reports its own failures; the suggestion
field is where the recovery hint lives.

| Namespace | Domain |
| --- | --- |
| `backup.*` | Backup pipeline (`backup.failed`, `backup.encrypt_no_kek`, `backup.kek_load_failed`, `backup.kms_open_failed`, `backup.compare.*`, `backup.delete.*`, `backup.undelete.*`) |
| `restore.*` | Restore pipeline (`restore.failed`, `restore.kek_mismatch`, `restore.kek_resolve_failed`, `restore.target_in_wal_gap`, `restore.unknown_scheme`) |
| `wal.*` | WAL streaming / fetch (`wal.slot_missing`, `wal.slot_create_failed`, `wal.slot_repair_failed`, `wal.fetch.*`, `wal.gap_purge_failed`, `wal.push_failed`, `wal.stream_error`) |
| `repo.*` | Repo lifecycle, GC, scrub, replicate (`repo.open_failed`, `repo.gc.*`, `repo.scrub.*`, `repo.check.*`, `repo.replicate.*`, `repo.wal_prune.failed`, `repo.wipe.partial`) |
| `repo.replicate.incomplete` | `repo replicate` finished but the destination is NOT a complete replica (some manifests/chunks failed or are missing). Non-zero exit so `replicate && rm source` can't trust a partial DR copy — re-run until it exits 0, then `repo replicate verify`. |
| `repair.*` | Manifest / attestation / chunk repair |
| `kms.*` | KMS rotate / shred / verify (`kms.rotate_failed`, `kms.shred_failed`, `kms.verify_failed`); `kms.unreachable` is the only leaf that maps to exit 8 |
| `chain.*` | Backup-chain integrity (`chain.cycle`, `chain.too_deep`, `chain.no_full_anchor`, `chain.broken_tombstoned`, `chain.degenerate`, `chain.missing_pg_manifest`) |
| `audit.*` | Audit log (`audit.append_failed`, `audit.anchor_failed`, `audit.verify_failed`, `audit.search_failed`, `audit.export_bundle_failed`, `audit.summary_failed`) |
| `approval.*` | n-of-m approval flow |
| `threshold.*` | Threshold-signing (FROST) ceremony |
| `dsa.*` | Detached signing authority |
| `roster.*` (under `notfound`/`conflict`) | Operator roster |
| `agent.*`, `server.*` | Agent / control-plane bring-up |
| `cost.*`, `forecast.*`, `capacity.*` | Reporting and forecasting |
| `db.*` | PG client install / uninstall |
| `gameday.*`, `recovery.*` | Disaster-recovery drills |
| `dispatch.*` | Job dispatcher |
| `redact.*` | Logical redaction passes |
| `partial.*` | Partial / table-level restore |
| `combine.*` | `pg_combinebackup` orchestration |
| `paths.*`, `init.*`, `config.*` | Bootstrap |
| `compliance.*`, `integrity.*`, `insider.*` | Compliance / integrity scanning |
| `llm.*` | LLM provider, skill loading, MCP server |
| `history.*` | Restore-history slice |
| `hold.*`, `rotate.*`, `jit.*` | Legal hold, KMS rotation, JIT credentials |
| `notify.*` | Sink configuration |
| `logical.*` | Logical replication slots / streams |
| `fleet.*` | Fleet inventory |
| `patroni.*`, `pg.*` | Patroni / PG identification |
| `connect.*`, `deployment.*` | Connection probing |
| `standby.*`, `timetravel.*`, `timetable.*` | Standby / timetravel features |
| `gameday.*` | Disaster drills |
| `internal` | Catch-all for unstructured errors funnelled through `output.ToError` |

## See also

- [Exit codes](exit-codes.md) — the namespace → exit
  mapping and the resolution algorithm.
- [Output event schema](output-event-schema.md) — the
  surface every error rides on.
- [Operations: troubleshooting](../operations/troubleshooting.md)
  — leaf-by-leaf recovery walkthroughs for the most common
  errors.
