---
title: Data residency pinning
description: Per-deployment region constraints enforced via pg_hardstorage residency.
tags:
  - residency
  - sovereignty
---

# Data residency pinning

Data residency pinning constrains a deployment's repository
to a set of allowed regions. The compliance primitive for
"this deployment's backups MUST stay in {EU}" — a common
ask from EU-regulated data, customer-data-segregation
clauses, and US Federal procurement.

The companion primitives are
[hold](../operations/operator-guide.md#3-retention) (pin
individual backups against retention) and `classify` (tag
the data's sensitivity).

---

## Lead with the command

```sh
pg_hardstorage residency set db1 eu
pg_hardstorage residency set db2 eu-west-1 eu-central-1
pg_hardstorage residency list
pg_hardstorage residency check db1
pg_hardstorage residency clear db1
```

`set` writes the residency policy onto the deployment
config. `check` validates the configured repo's region
against the policy. `list` returns all configured
residencies fleet-wide. `clear` removes the constraint.

---

## Match rules

Match is **case-insensitive, hyphen-aware prefix**:

| Policy | Matches | Doesn't match |
| --- | --- | --- |
| `["eu"]` | `eu-west-1`, `eu-central-1`, `eu-north-1` | `us-east-1`, `eumetsat` (no hyphen boundary) |
| `["eu-west-1"]` | `eu-west-1` only | `eu-west-2`, `eu-central-1` |
| `["eu", "uk"]` | Anything in EU or UK | `us-*`, `ap-*` |
| `[]` (empty) | No constraint (default) | — |

The hyphen boundary is the safety: `"eu"` does not
accidentally match `"europa"` or any other region whose
name happens to start with `e`+`u`.

---

## Storage plugin region detection

Storage plugins implement an optional `Region()` method via
the `RegionAware` interface. The S3 plugin parses
`?region=...` from the repo URL or pulls from the
endpoint. Azure Blob and GCS plugins return their
configured region.

The **filesystem plugin reports the empty string**
(`region unknown`) and FAILS any non-empty residency
check. Local-disk repos can't enforce residency, and
silently treating that as a pass would defeat the purpose.
If you genuinely don't care about residency, leave the
policy empty (`[]`).

---

## Configuration shape

Direct YAML edit:

```yaml
deployments:
  db1:
    pg_connection: postgres://pgbackup@db1.example.com/postgres
    repo: 's3://acme-eu-backups/?region=eu-central-1'
    residency: ["eu"]
    classification: confidential

  db2:
    pg_connection: postgres://pgbackup@db2.example.com/postgres
    repo: 's3://acme-us-backups/?region=us-east-1'
    residency: ["us"]
```

A drift between `repo` and `residency` (e.g. a `repo` whose
region falls outside `residency`) surfaces in `doctor`:

```console
$ pg_hardstorage doctor db1
db1 — PG 17.2
  ✗ residency: configured region 'us-east-1' violates policy ['eu']
    Suggested fix:
      Either update the repo to an EU region:
        pg_hardstorage deployment edit db1 \
            --repo 's3://acme-eu-backups/?region=eu-central-1'
      Or update the policy:
        pg_hardstorage residency set db1 us
```

---

## Today: an operator-run check

`residency check` is read-only — it validates the
configured repo's region against the policy and reports
the result. **Automatic enforcement at backup-commit time
is a v1.0 lift**: the runner needs a residency gate before
`pg_backup_start`. v0.5 ships the surface and the `doctor`
integration that fires before the backup runs.

For v0.1, the safety is layered:

- `doctor` flags a residency violation as a refusal-class
  finding.
- The deployment edit / repo init flow records the residency
  policy and surfaces it in the `deployment list` output
  and the audit chain.
- An operator who configures a violating repo is responsible
  for re-checking after configuration changes.

For v1.0+ the planned enforcement is:

- `residency check` becomes part of the pre-flight refusal
  gate; backup commit refuses with `verify.residency_violation`
  (exit 4) when the configured repo's region is outside the
  policy.
- The audit chain records every residency check + violation
  attempt.

---

## Cross-region replication

A deployment with residency `["eu"]` whose repo is also
async-replicated to a US region for DR is a policy
violation at the replica side. The replication subsystem
respects per-deployment residency:

- `repo replicate --from <eu-repo> --to <us-repo>` for a
  deployment with `residency: ["eu"]` refuses with
  `verify.residency_violation` unless explicitly
  overridden via `--allow-cross-region`.
- The `--allow-cross-region` flag is recorded in the audit
  chain — an explicit operator decision, not an
  invisible default.

---

## Mapping to compliance frameworks

| Framework | Control / requirement | How residency satisfies |
| --- | --- | --- |
| GDPR | Art. 44–50 — international transfers | EU pinning prevents cross-border transfer; audit chain records the policy |
| Schrems II | Adequacy decisions | Region pinning to "EU" excludes US-headquartered providers' US regions |
| HIPAA | §164.308(b) — Business Associate Contracts | BA-only regions enforced via residency |
| FedRAMP | SC-7 — Boundary Protection | Region pinning to GovCloud (`us-gov-*`) regions |
| ISO 27001 | A.5.34 — Privacy and protection of PII | Geographic constraint per regulatory zone |

---

## Operational flow

1. **Decide policy** per deployment based on the data's
   regulatory zone (data-classification + jurisdiction).
2. **Apply** via `residency set <deployment> <region>...`.
3. **Verify** via `residency check <deployment>`.
4. **Audit** — the residency policy is stored in the deployment
   config (it is not emitted as an audit-chain event); a *violation*
   at backup commit is recorded as `verify.residency_violation`.
5. **Re-verify** at every config change — `doctor` catches
   drift on its next run.

---

## Caveats

- The check inspects the **configured repo's reported
  region**, not the actual physical hosting region. A
  storage plugin pointing at a CDN edge that mirrors data
  to multiple regions will report the primary region only
  — the operator owns confirming the underlying replication
  topology matches the policy.
- Residency does NOT guarantee data sovereignty against
  legal-process compulsion. A US-based cloud provider can
  be legally compelled to disclose data hosted in their
  EU region under CLOUD Act provisions. The technical
  pinning is a control; the legal posture is your counsel's
  problem.
- Air-gapped repos report no region; combine with
  `airgap: strict` mode to enforce "no network egress at
  all" instead of region-prefix matching.

---

## Further reading

- [Source: `internal/cli/residency.go`](https://github.com/cybertec-postgresql/pg_hardstorage/blob/main/internal/cli/residency.go)
- [Operator guide: holds](../operations/operator-guide.md#3-retention)
  — companion primitive for backup-level pinning.
- [SOC 2 control mapping](soc2-control-mapping.md) — A1.2,
  CC9.1.
- [GDPR Art. 17 crypto-shred](gdpr-art-17-crypto-shred.md)
  — the erasure half of the EU residency story.
