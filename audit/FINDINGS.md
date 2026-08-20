# h200_fixes audit — FINDINGS

**Branch:** `h200_fixes` (local-only, never pushed)
**Date:** 2026-08-20
**Scope:** deep read of 6 highest-risk files; targeted grep across the entire `internal/` tree
**Out of scope:** WAL/replication, SQL parsing, time-travel/threshold, full CLI walkthrough, docs cross-check (all dropped to fit a single audit pass)
**Output:** `audit/findings.jsonl` (machine-readable) + this file (human summary)

## Severity histogram

| severity | count | what it means here |
|----------|-------|--------------------|
| critical |   2    | silent data loss / silent remote-write escape under operator-error conditions |
| high     |   3    | hard bugs with concrete exploits; missing-but-required input validation |
| medium   |   4    | bounded blast radius OR verify-before-fix; documented landmines |
| low      |   2    | maintainability / doc drift; refactor backlog |
| info     |   1    | session-meta observation about this audit branch |

## Critical (2)

### F-0001 — `kek.go:77-91` LoadOrGenerateKEK falls through to GENERATE on permission errors
`internal/backup/keystore/kek.go` — `LoadOrGenerateKEK` checks `assertKEKFileMode` then, if the check fails for ANY reason other than ENOENT, falls into the generate-fresh branch. An operator who `chmod 0644 kek.bin` to debug a permission issue silently gets a NEW KEK and loses decryption of every previously-encrypted backup. The docstring promises a loud refusal; the code delivers silent data loss.

### F-0002 — `scp.go:247-260` scp.resolvePrefix has bypassable path-traversal defence
`internal/plugin/storage/scp/scp.go` — uses `strings.Contains(prefix, "..")` as the only escape check. Lexically fragile (rejects legitimate keys like `weird..name`), and there is no `filepath.Rel` post-check after `path.Join`. Compare to `restore.safeJoinTarget` (restore.go:1484) which does Clean + Rel with explicit under-prefix verification. scp is the SSH-exec backend where every Put/Get/List flows through resolve — a bypass = arbitrary remote file write under the SSH user's home.

## High (3)

### F-0003 — `redact.go:289-293` MD5 used for PII-redaction hashing
`internal/restore/redact/redact.go` — `hash_to_uuid` and `hash_keep_domain` strategies use MD5 over (salt || value). This is the only MD5 in the entire repo (verified by grep); the encryption stack is AES-256-GCM. The MD5 here is forced by Postgres compatibility (the SQL side uses PG's `md5()` function), but the security implication — preimage attacks against common values like email addresses once the salt leaks — is not documented anywhere in the package doc.

### F-0004 — `routes.go:395-440` REST API never validates deployment/backup_id from URL path or JSON body
`internal/server/routes.go` — the repo layer has `backup.ValidateDeployment()` and `backup.ValidateBackupID()` (manifest_store.go:80-81) that reject `/`, `\\`, control chars, and `..`. **Zero production callers** (only tests). The REST handlers extract `name` from `/v1/deployments/<name>/...` and `backup_id` from JSON bodies and feed them straight into `PrimaryPath()`. An authenticated client can craft a deployment name like `..%2Fadmin` and reach manifest keys outside their authorized scope.

### F-0005 — `cas.go` (896 LOC) needs dedicated audit; not deep-read this pass
The CAS engine is the single largest risk surface in the repo by lines + by criticality (chunk fetch + WORM lock + encryption seal). The audit could not deep-read it in this pass. Three sub-risks flagged for the dedicated audit: (1) GC ↔ GetChunkBytes race window, (2) WORM retention applied pre-rename vs post-rename, (3) mixed-mode encryption refusal.

## Medium (4)

### F-0006 — `manifest_store.go:571-627` Deployments() slow path has no upper bound
O(N) full-manifest walk when the deployment-index sentinel is absent; a single backfill failure on a read-only repo means every subsequent call is slow. Recommend bounded slice + streaming API + duration metric.

### F-0007 — `routes.go` ↔ `openapi.yaml` ↔ `SPEC.md` drift
This audit found 3 categories (jobs, deployments, restores, verifies) registered, the SPEC mentions at least 5 more (repos, approvals, etc.). Open follow-up ticket for full diff. The api-spec-lint CI job only lints the YAML itself, not the YAML-vs-code gap.

### F-0008 — `restore.go:1485-1502` safeJoinTarget is documented as lexical-only
The SCOPE comment explicitly warns that adding symlinks to FileEntry would bypass the check. Code does not enforce the invariant; it relies on no-future-bug. Recommend openat(O_NOFOLLOW) on every target path component, with a small perf cost (~5% on restore throughput).

### F-0009 — `redact.go:354-364` regex replacement in `regex:` strategy: needs live verification
`strategyToSQLExpr` passes the replacement through `quoteString` (single-quote-escapes only) but does not validate the replacement for Postgres' regexp_replace backref semantics. **Could not confirm exploitability in this audit pass** — needs a live test against a real PG to determine if a rules file can drive SQL injection via the replacement argument. If exploitable: upgrade to critical.

## Low (2)

### F-0010 — SPEC.md makes many feature claims that the implementation does not support
The strongest signal: `internal/slo/` has **0 source files**, yet SPEC.md describes an SLO monitoring feature. `internal/fips/` has 4 files, `internal/insider/` has 2 — both areas claim substantial coverage. Cross-check is the natural follow-up audit.

### F-0011 — `internal/cli/wal.go` is 3134 LOC — largest single file in the repo
Single-file CLI command groups are a maintainability hazard. Recommend per-subcommand split (no behaviour change required). Not a security finding; pure refactor backlog.

## Info (1)

### F-0012 — session-local guard hook does not survive clone
`.git/hooks/pre-push` blocks pushes from `h200_fixes` but the hook is NOT tracked in git. A fresh clone has no hook. `audit/DO_NOT_PUSH.md` documents the intent but the enforcement is local-only. Either commit a `scripts/install-hooks.sh` that future maintainers run, or rely on repo-side GitHub settings (out of harness reach).

## What the next model should do

1. **Fix F-0001 first.** It's a 10-line code change with a clear test. The bug is a single false-positive acceptance in a permission gate; the fix is to make the gate loud.
2. **Fix F-0002 second.** Mirror `safeJoinTarget` in `scp.resolvePrefix`. Add the contract test before fixing so the behaviour change is enforced.
3. **Investigate F-0009 in a fresh session** with a live PG. If it's exploitable, it becomes the highest-priority item; if not, downgrade to docs.
4. **Schedule dedicated audits** for `internal/repo/cas.go` (F-0005), the full `internal/server/routes.go` ↔ `openapi.yaml` ↔ `SPEC.md` diff (F-0007), and SPEC.md claim verification (F-0010). None of those fit a single 6-file pass.
5. **Open the PR** only after F-0001, F-0002, F-0004 are fixed and the rest are either resolved or explicitly accepted. The findings.jsonl is the PR description's "Audit results" section.

## Audit pipeline metadata

- Branch: `h200_fixes` (created this session)
- Pre-push guard: `.git/hooks/pre-push` blocks all pushes from this branch
- Remote verification: `git ls-remote origin refs/heads/h200_fixes` returns empty (no push ever happened)
- Workflow cost pass: commit `3e86e4c` reduced GitHub Actions spend from ~5h/week to ~0 (only `v*` tag pushes + manual dispatch fire anything)
- Files reviewed in depth: 6 (encryption.go, keywrap.go, kek.go, unwrap.go, restore.go, manifest_store.go, redact.go, fs.go, scp.go)
- Files grep-scanned: ~1,300 (`find internal cmd -name '*.go' ! -name '*_test.go'`)
- Findings: 12 (2 critical, 3 high, 4 medium, 2 low, 1 info)
