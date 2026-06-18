---
title: SOC 2 control mapping
description: AICPA Trust Services Criteria mapped to pg_hardstorage features, commands, and audit-event codes.
tags:
  - soc2
  - controls
---

# SOC 2 control mapping

`pg_hardstorage` maps to the AICPA Trust Services Criteria
(TSC) categories: Security (CC), Availability (A),
Processing Integrity (PI), Confidentiality (C), and Privacy
(P). The mapping below is the assessor's quick-reference;
the canonical source is the
[`pg_hardstorage compliance report`](../operations/operator-guide.md)
output, which auto-generates this matrix per repo per
window.

The framework string in the report's JSON is `soc2`.

---

## Common Criteria — Security

| Control | Description | Product feature | Command | Audit event |
| --- | --- | --- | --- | --- |
| CC6.1 | Logical access security | Three-layer envelope encryption (RKEK → BDEK → per-chunk Kc) | `pg_hardstorage backup ...` | `backup.create` |
| CC6.6 | Logical access — boundary protection | Storage plugin per-region scoping; air-gap mode rejects public endpoints | `pg_hardstorage residency set ...` | (config) |
| CC6.7 | Restricted access to information assets | Per-tenant KEK + RBAC scopes | `pg_hardstorage rbac ...` | `rbac.*` |
| CC6.8 | Detection and prevention of unauthorised software | SLSA L3 build provenance + cosign signatures on every release | (build-time) | (cosign attest) |
| CC7.1 | Detection of system anomalies | Anomaly score (Z-score over 30-day baseline) per backup | `pg_hardstorage backup ...` | `anomaly.detected` |
| CC7.2 | System events logged in tamper-evident chain | Hash-chained Merkle audit log | `pg_hardstorage audit verify-chain` | (every event) |
| CC7.3 | Evaluation of detected events | Insider-threat scanner + audit search | `pg_hardstorage insider scan` | `insider.scan` |
| CC7.4 | Incident response and containment | Runbooks R1–R7; structured `doctor` remediation | `pg_hardstorage doctor` | `doctor.suggested_fix` |
| CC8.1 | Authorise and document changes | n-of-m approval workflow on destructive ops | `pg_hardstorage approval request ...` | `approval.request`, `approval.approve` |
| CC9.1 | Risk identification + classification | Data classification tags drive retention + residency floor | `pg_hardstorage classify set ...` | (manifest tag) |
| CC9.2 | Vendor / business partner management | Storage plugin Tier-1 / Tier-2 boundary; signed plugins | (build-time) | (cosign attest) |

---

## Availability

| Control | Description | Product feature | Command | Audit event |
| --- | --- | --- | --- | --- |
| A1.1 | Capacity for current and future commitments | Capacity report with 30/90/365-day projection | `pg_hardstorage capacity report` | (read-only) |
| A1.2 | Backup processes monitored + verified | Verifier subsystem + post-restore `pg_verifybackup` gate | `pg_hardstorage verify ...` | `verify.run` |
| A1.3 | Backups replicated to redundant storage | Cross-region replication; replica completeness in compliance report | `pg_hardstorage repo replicate ...` | `replicate.completed` |

---

## Processing Integrity

| Control | Description | Product feature | Command | Audit event |
| --- | --- | --- | --- | --- |
| PI1.1 | Inputs validated | Manifest signature check on every read | (automatic) | `verify.manifest_signature` (on fail) |
| PI1.4 | Errors detected and corrected | End-to-end checksums; chunk SHA round-trip on read | (automatic) | `verify.scrub_mismatch` (on fail) |
| PI1.5 | Data retention per policy | GFS retention with WORM enforcement; tombstone soft-delete | `pg_hardstorage rotate ...`, `pg_hardstorage hold add ...` | `backup.rotate_delete`, `hold.add` |

---

## Confidentiality

| Control | Description | Product feature | Command | Audit event |
| --- | --- | --- | --- | --- |
| C1.1 | Confidential information identified | Data classification tags | `pg_hardstorage classify set ...` | (manifest tag) |
| C1.2 | Confidential information disposed of | Crypto-shred destroys per-tenant KEK | `pg_hardstorage kms shred ...` | `kms.shred` |

---

## Privacy

| Control | Description | Product feature | Command | Audit event |
| --- | --- | --- | --- | --- |
| P4.2 | Personal information disposed of | Per-tenant KEK shred for GDPR Art. 17 | `pg_hardstorage kms shred ...` | `kms.shred` |
| P6.1 | Personal information transferred only with consent | Cross-region replication respects residency policy | `pg_hardstorage repo replicate ...` | `replicate.completed` |

---

## Generating the per-window assessment

```sh
pg_hardstorage compliance report \
    --repo s3://acme-backups/ \
    --since 2026-01-01 --until 2026-04-01 \
    -o markdown \
    > q1-2026-soc2.md
```

The Markdown output is a verdict table per control with
pass/fail/not-applicable status, evidence (a one-line
summary of what the verdict is based on), and a
remediation field for failed controls.

JSON form (for ingest into a GRC platform):

```sh
pg_hardstorage compliance report \
    --repo s3://acme-backups/ \
    --since 2026-01-01 --until 2026-04-01 \
    -o json \
    | jq '.result.body.controls.controls[]
          | select(.framework == "soc2")'
```

---

## What this mapping is NOT

- **Not a SOC 2 attestation.** The mapping is the
  assessor's quick-reference; the auditor still issues
  the report.
- **Not exhaustive.** SOC 2 has hundreds of point-of-focus
  items; this matrix covers the 18 controls
  `pg_hardstorage` directly addresses. A full assessment
  requires complementary controls in the operator's
  environment.
- **Not legal advice.** Mapping decisions are subject to
  the auditor's review.

---

## Further reading

- [Audit evidence bundles](audit-evidence-bundles.md) — the
  forensic-grade export feeding the audit chain control
  evidence.
- [GDPR Article 17 — crypto-shred](gdpr-art-17-crypto-shred.md)
  — privacy-section detail.
- [ISO 27001 control mapping](iso-27001-control-mapping.md)
  — Annex A cross-reference.
- [SLSA L3 provenance](slsa-l3-provenance.md) — supply-chain
  control evidence.
