---
title: HIPAA mapping
description: HIPAA Security Rule Technical Safeguards (45 CFR §164.308 / §164.312) mapped to pg_hardstorage features.
tags:
  - hipaa
  - controls
---

# HIPAA mapping

HIPAA Security Rule Technical Safeguards (45 CFR §164.308
Administrative Safeguards and §164.312 Technical
Safeguards) mapped to `pg_hardstorage` features.

The framework string in the
[`compliance report`](../operations/operator-guide.md)
JSON is `hipaa`.

---

## §164.308 — Administrative Safeguards

| Section | Description | Product feature | Command | Audit event |
| --- | --- | --- | --- | --- |
| §164.308(a)(1)(ii)(D) | Information system activity review | Hash-chained audit log + `audit search` | `pg_hardstorage audit search ...` | (every event) |
| §164.308(a)(3)(ii)(C) | Termination procedures | Crypto-shred per-tenant KEK | `pg_hardstorage kms shred ...` | `kms.shred` |
| §164.308(a)(7)(ii)(A) | Data backup plan | Full backup pipeline | `pg_hardstorage backup ...` | `backup.create` |
| §164.308(a)(7)(ii)(B) | Disaster recovery plan | Runbooks R1–R7 + recovery drills | `pg_hardstorage recovery drill ...` | `recovery.drill_failed` (on fail) |
| §164.308(a)(7)(ii)(C) | Emergency mode operation plan | JIT tokens for break-glass elevation | `pg_hardstorage jit issue ...` | `jit.issue`, `jit.revoke` |
| §164.308(a)(7)(ii)(D) | Testing and revision procedures | Recovery drills with measured RTO | `pg_hardstorage recovery drill ...` | `recovery.drill_failed` (on fail) |
| §164.308(a)(7)(ii)(E) | Applications and data criticality analysis | Data classification tags drive retention floor + replication priority | `pg_hardstorage classify set ...` | (manifest tag) |
| §164.308(b)(1) | Business associate contracts and other arrangements | Per-tenant KEK + residency pinning constrain BA access | `pg_hardstorage residency set ...`, `kms shred ...` | (config), `kms.shred` |

---

## §164.312 — Technical Safeguards

| Section | Description | Product feature | Command | Audit event |
| --- | --- | --- | --- | --- |
| §164.312(a)(1) | Access control | Per-tenant KEK + RBAC scopes | `pg_hardstorage rbac ...` | `rbac.*` |
| §164.312(a)(2)(i) | Unique user identification | RBAC actor identity recorded on every event | (automatic) | (every event records `actor`) |
| §164.312(a)(2)(ii) | Emergency access procedure | JIT tokens + n-of-m approval | `pg_hardstorage jit issue ...`, `approval request ...` | `jit.issue`, `approval.request` |
| §164.312(a)(2)(iii) | Automatic logoff | JIT tokens auto-expire (max 24h) | (automatic) | `jit.issue` |
| §164.312(a)(2)(iv) | Encryption and decryption (Addressable) | AES-256-GCM per chunk (AES-256-GCM-SIV planned) | (automatic) | `backup.create` records `encryption.scheme` |
| §164.312(b) | Audit controls | Hash-chained Merkle audit log | `pg_hardstorage audit verify-chain` | (every event) |
| §164.312(c)(1) | Integrity | Ed25519-signed manifests + chunk SHA round-trip | `pg_hardstorage verify ...` | `verify.run` |
| §164.312(c)(2) | Mechanism to authenticate ePHI | Manifest signatures + audit chain hashing | `pg_hardstorage audit verify-chain` | `verify.manifest_signature` (on fail) |
| §164.312(d) | Person or entity authentication | mTLS for control plane ↔ agent; Ed25519 keypair per operator | (config) | (auth failures captured) |
| §164.312(e)(1) | Transmission security | TLS 1.2+ for storage backends; air-gap mode for offline networks | (config) | — |
| §164.312(e)(2)(i) | Integrity controls | End-to-end checksums; chunk SHA round-trip | (automatic) | `verify.scrub_mismatch` (on fail) |
| §164.312(e)(2)(ii) | Encryption (Addressable, transmission) | TLS 1.2+ on every storage backend; AES-256-GCM on the chunk itself | (config) | — |

---

## Encryption posture for ePHI

HIPAA's Addressable encryption requirements
(§164.312(a)(2)(iv) and §164.312(e)(2)(ii)) are satisfied
when:

| Layer | Requirement | `pg_hardstorage` posture |
| --- | --- | --- |
| At rest | NIST-validated cipher | AES-256-GCM (random 96-bit nonce) shipping today; AES-256-GCM-SIV (RFC 8452) is the planned default once a validated implementation lands. The `pg-hardstorage-fips` build uses AES-256-GCM (FIPS 140-validated via BoringCrypto) — BoringCrypto does not ship GCM-SIV, so the FIPS variant will likewise use GCM when GCM-SIV lands |
| In transit (storage backend) | TLS 1.2+ | All cloud backends; on-prem via plugin config |
| In transit (agent ↔ control plane) | TLS / mTLS | mTLS by default |
| Key management | NIST SP 800-57 lifecycle | KMS-backed RKEK; per-tenant KEK; KEK rotation with documented audit trail |

For "must-be-FIPS" environments, build the FIPS variant:

```sh
make build-fips
```

The FIPS variant refuses to start if `crypto/tls` reports
non-FIPS. `--fips-strict` panics on any non-FIPS plugin.

---

## Breach notification preparation

§164.404 — §164.412 (Breach Notification Rule) requires
identifying which ePHI was affected. The audit chain is
the discovery mechanism:

```sh
# Every backup of every PHI deployment in the breach window
pg_hardstorage audit search \
    --action-prefix backup. \
    --tenant phi-customers \
    --since 2026-04-01 --until 2026-04-15 \
    --repo s3://acme-backups/ \
    -o json \
    | jq '.result.body[] | .subject'
```

For affected-data scoping, the backup manifest's logical
bytes + table list lets the BA identify which records
were potentially exposed.

---

## Generating the per-window assessment

```sh
pg_hardstorage compliance report \
    --repo s3://acme-backups/ \
    --since 2026-01-01 --until 2026-04-01 \
    -o json \
    | jq '.result.body.controls.controls[]
          | select(.framework == "hipaa")'
```

---

## What this mapping is NOT

- **Not a HIPAA compliance attestation.** A covered entity
  or business associate must complete its own risk
  analysis (§164.308(a)(1)(ii)(A)) and adopt
  organisational policies separate from the technical
  controls below.
- **Not legal advice.** Mapping decisions are subject to
  the entity's privacy officer + counsel.

---

## Further reading

- [SOC 2 control mapping](soc2-control-mapping.md) —
  cross-reference.
- [GDPR Article 17 — crypto-shred](gdpr-art-17-crypto-shred.md)
  — termination procedure detail.
- [Audit evidence bundles](audit-evidence-bundles.md) — the
  audit-trail export for breach investigation.
