---
title: FedRAMP mapping
description: NIST 800-53 / FedRAMP Moderate baseline mapped to pg_hardstorage features.
tags:
  - fedramp
  - nist
  - controls
---

# FedRAMP mapping

FedRAMP Moderate baseline (NIST SP 800-53 Rev. 5) controls
that `pg_hardstorage` directly addresses. The Federal
authority-to-operate (ATO) flow requires an SSP (System
Security Plan) that maps every applicable control to an
implementation; this page is the input for the
"Information Backup" / "Audit and Accountability" /
"System and Communications Protection" sections of that
SSP.

The framework string in the
[`compliance report`](../operations/operator-guide.md)
JSON is `fedramp`.

---

## AC — Access Control

| Control | Title | Product feature | Command | Audit event |
| --- | --- | --- | --- | --- |
| AC-2 | Account management | RBAC scopes; per-tenant KEK; JIT tokens | `pg_hardstorage rbac ...`, `jit issue ...` | `rbac.*`, `jit.*` |
| AC-3 | Access enforcement | n-of-m approval gates on destructive ops | `pg_hardstorage approval request ...` | `approval.request`, `approval.approve` |
| AC-6 | Least privilege | RBAC scope per principal; JIT for break-glass elevation | `pg_hardstorage jit issue --scope ...` | `jit.issue` |
| AC-6(7) | Review of user privileges | `audit search --action-prefix rbac.` | `pg_hardstorage audit search ...` | `rbac.*` |
| AC-7 | Unsuccessful logon attempts | RBAC denials recorded | (automatic) | `auth.denied` |
| AC-12 | Session termination | JIT tokens auto-expire (max 24h) | (automatic) | `jit.issue` |

---

## AU — Audit and Accountability

| Control | Title | Product feature | Command | Audit event |
| --- | --- | --- | --- | --- |
| AU-2 | Audit events | Hash-chained Merkle log; every operator action is an event | `pg_hardstorage audit search ...` | (every event) |
| AU-3 | Content of audit records | Schema-versioned events with actor, timestamp, action, subject, body | (automatic) | (every event) |
| AU-4 | Audit storage capacity | Repo capacity report includes audit byte category | `pg_hardstorage capacity report` | (read-only) |
| AU-5 | Response to audit logging failures | Append failure surfaces via `defaultAppendErrorLogger`; missing head pointer logged | (automatic) | (logged to stderr) |
| AU-6 | Audit review, analysis, and reporting | Insider-threat scanner; `audit search`; `audit summary` | `pg_hardstorage insider scan`, `audit search`, `audit summary` | `insider.scan` |
| AU-9 | Protection of audit information | Hash chain + WORM bucket support | `pg_hardstorage audit verify-chain` | `verify.audit_chain_broken` (on fail) |
| AU-9(3) | Cryptographic protection | Ed25519 signature on audit evidence bundles | `pg_hardstorage audit export-bundle` | (bundle signed) |
| AU-10 | Non-repudiation | Hash-chained log links each event to the prior event | `pg_hardstorage audit verify-chain` | (every event) |
| AU-11 | Audit record retention | WORM retention propagates to audit events | (config) | (WORM-locked on write) |
| AU-12 | Audit generation | Every operator action emits an event automatically | (automatic) | (every event) |

---

## CP — Contingency Planning

| Control | Title | Product feature | Command | Audit event |
| --- | --- | --- | --- | --- |
| CP-2 | Contingency plan | Runbooks R1–R7 covering canonical disaster scenarios | (docs) | — |
| CP-4 | Contingency plan testing | Recovery drills with measured RTO | `pg_hardstorage recovery drill ...` | `recovery.drill_failed` (on fail) |
| CP-9 | System backup | Full backup pipeline | `pg_hardstorage backup ...` | `backup.create` |
| CP-9(1) | Testing for reliability and integrity | Verifier subsystem + post-restore `pg_verifybackup` | `pg_hardstorage verify ...` | `verify.run` |
| CP-9(3) | Separate storage for critical information | Cross-region replication | `pg_hardstorage repo replicate ...` | `replicate.completed` |
| CP-9(8) | Cryptographic protection | AES-256-GCM per chunk; Ed25519 manifest signatures | (automatic) | `backup.create` |
| CP-10 | System recovery and reconstitution | Restore command + sandbox `pg_verifybackup` gate | `pg_hardstorage restore ...` | `restore.completed` |

---

## IA — Identification and Authentication

| Control | Title | Product feature | Command | Audit event |
| --- | --- | --- | --- | --- |
| IA-2 | Identification and authentication of organizational users | mTLS for control plane ↔ agent; Ed25519 keypair per operator | (config) | (auth failures captured) |
| IA-5 | Authenticator management | KEK rotation; JIT tokens with bounded TTL | `pg_hardstorage kms rotate`, `jit issue ...` | `kms.rotate`, `jit.issue` |
| IA-7 | Cryptographic module authentication | FIPS variant uses BoringCrypto FIPS-validated module | (build) | — |

---

## SC — System and Communications Protection

| Control | Title | Product feature | Command | Audit event |
| --- | --- | --- | --- | --- |
| SC-7 | Boundary protection | Air-gap mode rejects public endpoints; storage plugin per-region scoping | `pg_hardstorage residency set ...` | (config) |
| SC-8 | Transmission confidentiality | TLS 1.2+ on all storage backends; mTLS control plane | (config) | — |
| SC-12 | Cryptographic key establishment and management | KMS-backed RKEK; per-tenant KEK | `pg_hardstorage kms ...` | `kms.*` |
| SC-13 | Cryptographic protection | AES-256-GCM (shipping today); AES-256-GCM-SIV planned, also for FIPS variant when GCM-SIV lands | (automatic) | `backup.create` |
| SC-28 | Protection of information at rest | Three-layer envelope encryption | (automatic) | `backup.create` |
| SC-28(1) | Cryptographic protection | AES-256-GCM per chunk; Ed25519 manifest sigs | (automatic) | `backup.create` |

---

## SI — System and Information Integrity

| Control | Title | Product feature | Command | Audit event |
| --- | --- | --- | --- | --- |
| SI-3 | Malicious code protection | Tier-1 plugin model + cosign-signed releases | (build) | — |
| SI-4 | System monitoring | Prometheus metrics; OpenTelemetry traces; insider-threat scanner | `pg_hardstorage insider scan` | `insider.scan` |
| SI-7 | Software, firmware, and information integrity | SLSA L3 build provenance + reproducible builds | (build-time) | (SLSA provenance via slsa-github-generator + cosign sign-blob) |
| SI-7(1) | Integrity checks | Continuous-attestation `integrity` runs over the repo | `pg_hardstorage integrity run` | `integrity.run` |
| SI-7(7) | Integration of detection and response | `doctor` emits structured remediation commands for operator action | `pg_hardstorage doctor` | `doctor.suggested_fix` |

---

## CM — Configuration Management

| Control | Title | Product feature | Command | Audit event |
| --- | --- | --- | --- | --- |
| CM-2 | Baseline configuration | `pg_hardstorage doctor` validates the resolved config | `pg_hardstorage doctor` | `doctor.issues_present` |
| CM-3 | Configuration change control | Deployment changes are recorded in the audit chain; other settings (SLO, etc.) live in config | `pg_hardstorage deployment edit ...` | `added` / `edited` / `removed` |
| CM-7 | Least functionality | Air-gap mode; FIPS-strict mode; explicit feature opt-in | (config) | — |

---

## FedRAMP-specific notes

### FIPS 140-3 validation

Build the FIPS variant for FedRAMP environments:

```sh
make build-fips
```

The variant uses `GOEXPERIMENT=boringcrypto` and links
against the BoringCrypto FIPS-validated module. Refuses to
start if `crypto/tls` reports non-FIPS. `--fips-strict`
panics on any non-FIPS plugin.

### FedRAMP High vs Moderate

This page covers the **Moderate** baseline. The High
baseline adds enhanced AU-9 (cryptographic + multiple
copies in physically separated locations), AC-6(10)
(prohibit non-privileged from executing privileged
functions), and stricter SC-7 boundary protections. The
existing controls satisfy these enhancements when paired
with multi-region replication and rigorous JIT scoping.

### GovCloud regions

Use `residency` to pin to GovCloud regions:

```sh
pg_hardstorage residency set fedramp-prod us-gov-east-1 us-gov-west-1
```

The replication subsystem refuses cross-boundary
replication unless explicitly overridden via
`--allow-cross-region` (which is itself audit-logged).

---

## Generating the SSP-input report

```sh
pg_hardstorage compliance report \
    --repo s3://acme-fedramp-backups/ \
    --since 2026-01-01 --until 2026-04-01 \
    -o markdown \
    > q1-2026-fedramp-controls.md
```

For the SSP, the Markdown is the per-control verdict +
evidence + remediation (where applicable). Pair with the
Audit Evidence Bundle for AU-9 / AU-11 evidence.

---

## What this mapping is NOT

- **Not an ATO.** A federal sponsor's authorising official
  issues the ATO; this matrix is one input.
- **Not a 3PAO assessment substitute.** The third-party
  assessor (3PAO) verifies each control's implementation;
  the matrix is for the SSP draft.
- **Not legal advice.** Mapping decisions are subject to
  the agency's authorising official + legal review.

---

## Further reading

- [SOC 2 control mapping](soc2-control-mapping.md) —
  cross-reference for SOC 2-certified deployments.
- [SLSA L3 provenance](slsa-l3-provenance.md) — SI-7
  evidence.
- [Audit evidence bundles](audit-evidence-bundles.md) —
  AU-9 / AU-11 export.
- [Data residency pinning](data-residency-pinning.md) —
  SC-7 enforcement.
