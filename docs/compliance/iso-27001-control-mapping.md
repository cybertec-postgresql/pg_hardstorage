---
title: ISO 27001 control mapping
description: ISO/IEC 27001:2022 Annex A controls mapped to pg_hardstorage features.
tags:
  - iso27001
  - controls
---

# ISO 27001 control mapping

ISO/IEC 27001:2022 Annex A controls mapped to
`pg_hardstorage` features, commands, and audit-event codes.

The framework string in the
[`compliance report`](../operations/operator-guide.md)
JSON is `iso27001`.

---

## A.5 — Organizational

| Control | Title | Product feature | Command | Audit event |
| --- | --- | --- | --- | --- |
| A.5.10 | Acceptable use of information | RBAC scopes + JIT tokens for break-glass | `pg_hardstorage jit issue ...` | `jit.issue`, `jit.revoke` |
| A.5.15 | Access control | Per-tenant KEK + RBAC scope | `pg_hardstorage rbac ...` | `rbac.*` |
| A.5.23 | Information security for use of cloud services | Storage plugin per-region scoping; air-gap mode | (config) | (config) |
| A.5.30 | ICT readiness for business continuity | Disaster runbooks R1–R7; recovery drills with measured RTO | `pg_hardstorage recovery drill ...` | `recovery.drill_failed` (on fail) |
| A.5.34 | Privacy and protection of PII | Data residency + classification | `pg_hardstorage residency set ...`, `classify set ...` | (config), (manifest tag) |

---

## A.6 — People

ISO 27001 A.6 controls are out-of-scope for a backup tool;
they cover personnel screening, training, etc. The audit
chain's `actor` field provides traceability for personnel
actions.

---

## A.7 — Physical

A.7 covers physical access controls; out-of-scope for
software. The repository's physical hosting is the
operator's choice (S3, on-prem, etc.) — `residency`
constrains the geography.

---

## A.8 — Technological

| Control | Title | Product feature | Command | Audit event |
| --- | --- | --- | --- | --- |
| A.8.10 | Information deletion | Tombstone soft-delete + GC; legal-hold pinning | `pg_hardstorage rotate --apply`, `pg_hardstorage repo gc --apply` | `backup.rotate_delete`, `repo.gc` |
| A.8.12 | Data leakage prevention | PII redaction in logs + LLM tools; air-gap mode | (automatic) | `redact.apply_failed` (on fail) |
| A.8.13 | Information backup | Full backup pipeline with verification | `pg_hardstorage backup ...`, `verify ...` | `backup.create`, `verify.run` |
| A.8.14 | Redundancy of information processing facilities | Cross-region replication; multi-source restore | `pg_hardstorage repo replicate ...` | `replicate.completed` |
| A.8.15 | Logging | Structured JSON logs + hash-chained audit log | `pg_hardstorage audit verify-chain` | (every event) |
| A.8.16 | Monitoring activities | Prometheus metrics; OpenTelemetry traces; insider-threat scan | `pg_hardstorage insider scan` | `insider.scan` |
| A.8.17 | Clock synchronisation | UTC timestamps everywhere; NTP recommended in operator guide | (operator) | (timestamp on every event) |
| A.8.21 | Security of network services | TLS 1.2+ for storage backends; mTLS for control plane ↔ agent | (config) | (network failures captured) |
| A.8.24 | Use of cryptography | AES-256-GCM-SIV + Ed25519 + SHA-256 + HKDF-SHA256 | (automatic) | `backup.create` records `encryption.scheme` |
| A.8.25 | Secure development life cycle | SLSA L3 build provenance; reproducible builds verified weekly | (build-time) | (cosign attest) |
| A.8.26 | Application security requirements | Static analysis + race detector + sanitizers in CI | (build-time) | — |
| A.8.27 | Secure system architecture and engineering principles | Documented threat model + design north stars in `SPEC.md` | — | — |
| A.8.28 | Secure coding | Plugin sandboxing; n-of-m approval gating; refuse-don't-corrupt | `pg_hardstorage approval request ...` | `approval.request` |
| A.8.31 | Separation of development, test, and production environments | Tenant boundary enforced by per-tenant KEK | (config) | — |

---

## A.9 — Supplier relationships

Out-of-scope for the binary itself. SLSA L3 build
provenance covers the supply chain inputs to the binary
(see [SLSA L3 provenance](slsa-l3-provenance.md)).

---

## Generating the per-window assessment

```sh
pg_hardstorage compliance report \
    --repo s3://acme-backups/ \
    --since 2026-01-01 --until 2026-04-01 \
    -o markdown \
    | grep -A1 'iso27001'
```

JSON form:

```sh
pg_hardstorage compliance report \
    --repo s3://acme-backups/ -o json \
    | jq '.result.body.controls.controls[]
          | select(.framework == "iso27001")'
```

---

## ISMS scope statement

When `pg_hardstorage` is in the ISMS scope, the
information-asset register entry should record:

| Field | Value |
| --- | --- |
| Asset | `pg_hardstorage` backup repository at `s3://...` |
| Owner | (operator org) |
| Classification | Per-deployment via `classify` |
| Encryption | AES-256-GCM-SIV per chunk (FIPS variant: AES-256-GCM) |
| Retention | Per `retention` policy in deployment config |
| Audit | Hash-chained Merkle log; quarterly bundle export |
| Region | Per `residency` policy |

The audit evidence bundle is the assets-and-controls
evidence trail.

---

## Mapping to A.5.30 (business continuity)

ISO 27001 A.5.30 ICT readiness for business continuity is
explicitly addressed by the runbook + drill cadence:

| Activity | `pg_hardstorage` artefact |
| --- | --- |
| Recovery procedures documented | [Runbooks R1–R7](../reference/runbooks/index.md) |
| Recovery procedures tested | `recovery drill` quarterly cadence |
| RTO/RPO declared | `slo set <deployment>` |
| Recovery measured | Drill records actual RTO + writes audit event |

The `pg_hardstorage recovery readiness` command produces a
single-page snapshot suitable for inclusion in the BCM
plan.

---

## Further reading

- [SOC 2 control mapping](soc2-control-mapping.md) — TSC
  cross-reference.
- [Audit evidence bundles](audit-evidence-bundles.md) — the
  ISMS evidence package.
- [SLSA L3 provenance](slsa-l3-provenance.md) — A.8.25
  evidence.
- [GDPR Article 17 — crypto-shred](gdpr-art-17-crypto-shred.md)
  — A.8.10 / A.5.34 detail.
