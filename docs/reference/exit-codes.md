<!-- AUTO-GEN candidate: reflect over internal/output.Exit* constants and codePrefixToExit() to emit this table; per docs/DOC_PLAN.md auto-generation map. -->
---
title: Exit codes
description: Stable process-level exit codes for pg_hardstorage and the structured-error namespaces that map to them.
tags:
  - reference
  - exit-codes
  - cli
---

# Exit codes

`pg_hardstorage` commits to a stable, scriptable exit-code
contract.  Values come from the `Exit*` constants in
[`internal/output/exitcode.go`](https://github.com/cybertec-postgresql/pg_hardstorage/blob/main/internal/output/exitcode.go);
the table below is the v1.0 wire contract.  Values do not
change without a major-version bump.

## Codes

| Code | Constant | When it fires | Retry safe? |
| --- | --- | --- | --- |
| **0** | `ExitOK` | Command completed successfully. | n/a |
| **1** | `ExitError` | Generic failure. Default for unclassified errors and for any structured error whose code prefix is not in the namespace map below. | Maybe — re-run with `-o json` and inspect the structured error body to classify. |
| **2** | `ExitMisuse` | Bad CLI arguments or usage error: unknown flag, missing required flag, invalid value. Wraps `output.ErrUsage`; cobra-internal errors are remapped here. | No — fix the invocation. |
| **3** | `ExitAuth` | Authentication or authorization failure. Code namespace `auth.*`. | No — re-authenticate. |
| **4** | `ExitPreflight` | A pre-flight check refused the operation. **No mutation occurred.** Code namespace `preflight.*`. | Yes — once the underlying condition is fixed (free disk, PG version, target dir empty, etc.). |
| **5** | `ExitAborted` | Operation aborted by the user or by a context cancellation. Code namespace `aborted.*`. | Yes. |
| **6** | `ExitNotFound` | Named resource (backup, deployment, repo, sink, slot, …) does not exist. Code namespace `notfound.*`. | No — list and pick a real ID. |
| **7** | `ExitConflict` | Conflict: lease held, ID collision, in-progress operation, repo read-only, chain has live descendants. Code namespace `conflict.*`. | Yes — once the holder releases or you pick a new name. |
| **8** | `ExitUnreachable` | Storage backend or KMS provider is unreachable. Specifically the leaf codes `storage.unreachable` and `kms.unreachable`; other `storage.*` / `kms.*` codes stay in `ExitError`. | Yes — transient by nature. |
| **9** | `ExitVerifyFailed` | Verification failed (`verify.*` namespace) **or** an anomaly was detected (`anomaly.*` namespace). Same exit code so a single cron contract — "non-zero if anything is unusual" — covers both. | Investigate before retry. |
| **10** | `ExitDoctorIssues` | `pg_hardstorage doctor --exit-on-issues` found at least one issue. Code namespace `doctor.*`. | n/a — informational. |

## Code namespace → exit-code mapping

`*output.Error` carries a dotted, lowercase code (e.g.
`wal.slot_missing`, `auth.denied`, `verify.checksum_mismatch`).
The dispatcher walks the wrapped error chain via `errors.As`,
extracts the structured code, and routes the first dotted
segment to an exit code:

| Namespace prefix | Exit code |
| --- | --- |
| `auth.*` | `3` |
| `usage.*` | `2` |
| `preflight.*` | `4` |
| `aborted.*` | `5` |
| `notfound.*` | `6` |
| `conflict.*` | `7` |
| `verify.*` | `9` |
| `anomaly.*` | `9` |
| `doctor.*` | `10` |
| `storage.unreachable` (leaf) | `8` |
| `kms.unreachable` (leaf) | `8` |
| `restore.target_unreachable` (leaf) | `7` |
| `restore.target_in_wal_gap` (leaf) | `7` |
| any other code | `1` |

Unmatched namespaces fall through to `ExitError` (1) by
design — that is the safe default.  See
[Error codes](error-codes.md) for the full catalogue of
codes; that page is grouped by domain and pairs each code
with its typical recovery path.

## Resolution algorithm

`output.ExitCodeFor(err)` resolves an error to an exit code in
this order:

1. `err == nil` → `0`.
2. `errors.Is(err, output.ErrUsage)` → `2`.
3. `errors.As` finds a `*output.Error` in the chain → table
   above on `Error.Code`.
4. Otherwise → `1`.

Only structured `*output.Error` values can claim a
non-generic exit code.  Ad-hoc `errors.New` returns from
deep packages stay in the generic-error bucket — by design,
to keep the contract small.

## Scripting recipes

Cron jobs and CI pipelines should branch on the exit code
rather than parse stderr:

```bash
pg_hardstorage backup db1 --output ndjson | tee backup.log
case $? in
  0)  echo "ok" ;;
  4)  echo "pre-flight refused; no mutation" ;;
  5)  echo "aborted; safe to retry" ;;
  8)  echo "transient; retry with backoff" ;;
  9)  exit 1 ;; # alert: verify or anomaly
  *)  exit 2 ;; # everything else: investigate
esac
```

For verify cron, the same posture as `borg check` /
`restic check`: any non-zero exit triggers an alert.

## See also

- [Error codes](error-codes.md) — the full catalogue
  grouped by domain.
- [Output event schema](output-event-schema.md) — the
  surface every error rides on.
- [Operations: monitoring](../operations/monitoring.md) —
  cron exit-code wiring for unattended runs.
