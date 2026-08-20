# Scope Inventory

Generated 2026-08-20. 13 parallel scouts, one per scope key. Each scout writes `audit/raw/<scope-key>.jsonl`.

Size distribution (Go LOC, non-test source):

| scope-key                | paths                                                                                              | LOC     | priority lenses |
|--------------------------|----------------------------------------------------------------------------------------------------|---------|-----------------|
| cmd-entrypoints          | `cmd/`                                                                                             |   5.3k  | bug, security   |
| cli-config               | `internal/cli/`, `internal/cli/cmdtree/`, `internal/config/`, `internal/output/`, `internal/fsutil/`, `internal/paths/`, `internal/i18n/`, `internal/version/` |  very large | bug, docs, false-claim |
| backup-partial-logical   | `internal/backup/`, `internal/partial/`, `internal/logical/`, `internal/chain/`, `internal/backup/keystore/`, `internal/backup/retention/`, `internal/backup/runner/`, `internal/recovery/`, `internal/dbext/` |  large  | corruption, unsafe, mem |
| restore-verify-recovery  | `internal/restore/`, `internal/verify/`, `internal/restore/naturaltime/`, `internal/restore/postverify/`, `internal/restore/walfetchcmd/` |  large  | corruption, bug, unsafe |
| repo-storage-wal         | `internal/repo/`, `internal/repo/sharedkey/`, `internal/plugin/storage/`, `internal/wal/`, `internal/wal/inventory/`, `internal/wal/follower/`, `internal/wal/gapstate/`, `internal/pg/walsink/` |  large  | corruption, security, perf |
| pg-integration           | `internal/pg/`, `internal/pg/replication/`, `internal/pg/logicalreceiver/`, `internal/standby/`, `internal/patroni/` |  medium | bug, unsafe, docs |
| ops-policy               | `internal/server/`, `internal/agent/`, `internal/audit/`, `internal/obs/`, `internal/obs/metrics/`, `internal/fips/`, `internal/kms/`, `internal/dsa/`, `internal/integrity/`, `internal/invariant/`, `internal/approval/`, `internal/airgap/`, `internal/insider/`, `internal/repoaudit/`, `internal/scim/`, `internal/compliance/`, `internal/anomaly/`, `internal/slo/`, `internal/fleet/` |  medium-large | security, unsafe, false-claim |
| time-threshold-testkit-llm | `internal/timetravel/`, `internal/threshold/`, `internal/schedule/`, `internal/capacity/`, `internal/forecast/`, `internal/testkit/`, `internal/testkit/*`, `internal/llm/`, `internal/llm/*`, `internal/gameday/`, `internal/regression/`, `internal/plugin/llmprovider/`, `internal/simple/`, `internal/simple/prompt/`, `internal/jit/`, `internal/cost/` |  large  | bug, docs, perf |
| ext-sql                  | `ext/pg_hardstorage_extension/`                                                                    |   0.3k  | corruption, security |
| compat-proto-api         | `compat/`, `proto/`, `api/openapi.yaml`                                                            |   8.5k  | bug, security, docs |
| scripts-shell            | `scripts/`, `run_*.sh`, `compile.sh`                                                               |   3.2k  | unsafe, security |
| charts-packaging-deploy  | `charts/`, `packaging/`, `debian/`, `completions/`, `deploy/`, `dockerfiles/`, `share/skills/`     |  ~600kB | security, bug |
| docs-claims              | `docs/`, `SPEC.md`, `README.md`, `CHANGELOG.md`, `mkdocs.yml`, `SECURITY.md`, `CONTRIBUTING.md`    |  ~3MB   | false-claim, docs |

## Allocation

- Each scout receives an explicit `paths` list above.
- No scout is allowed to read or write outside its paths, except `docs-claims` which cross-references code (read-only).
- Aggregator (single sequential pass) merges + dedupes + ranks.

## Branch policy

All audit artefacts land on branch `h200_fixes` (currently checked out). **No remote push.** The next model opens the PR.
