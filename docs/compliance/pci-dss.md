---
title: PCI DSS mapping
description: PCI DSS v4.0 requirements 3, 9, and 10 mapped to pg_hardstorage features.
tags:
  - pci-dss
  - controls
---

# PCI DSS mapping

PCI DSS v4.0 requirements that apply to backup data
(Req. 3 — Protect Stored Account Data, Req. 9 — Restrict
Physical Access, Req. 10 — Log and Monitor Access) mapped
to `pg_hardstorage` features.

The framework string in the
[`compliance report`](../operations/operator-guide.md)
JSON is `pci_dss`.

PCI DSS scope is narrower than the general backup posture:
the controls below apply when backup data **contains
cardholder data (CHD) or sensitive authentication data
(SAD)**. For repos backing PCI-scope databases, every
cited control should match `pass` in the assessment
report.

---

## Req. 3 — Protect Stored Account Data

| Requirement | Description | Product feature | Command | Audit event |
| --- | --- | --- | --- | --- |
| 3.5.1 | PAN rendered unreadable | AES-256-GCM on every chunk; per-chunk key derivation | (automatic) | `backup.create` records `encryption.scheme` |
| 3.5.1.1 | Strong cryptography | AES-256-GCM (random 96-bit nonce) shipping today; AES-256-GCM-SIV (RFC 8452) planned | (automatic) | `backup.create` |
| 3.5.1.2 | Disk-level encryption alone is insufficient | Per-chunk encryption is in addition to any underlying disk encryption | (automatic) | `backup.create` |
| 3.6.1.1 | Cryptographic key custody | KMS-backed RKEK; per-tenant KEK; on-disk keyring with mode 0600 | `pg_hardstorage kms inspect` | `kms.*` |
| 3.6.1.2 | Cryptographic keys protected | Keys never appear in logs or output; `kms inspect` is read-only and shows only fingerprints | `pg_hardstorage kms inspect` | (read-only) |
| 3.6.1.3 | Key rotation | KEK rotation walks all manifests and rewraps DEKs | `pg_hardstorage kms rotate` | `kms.rotate` |
| 3.6.1.4 | Retired keys retained for retention period | Old KEKs preserved in keyring after rotation; `kms shred` is the explicit destruction path | `pg_hardstorage kms shred ...` | `kms.shred` |
| 3.7.1 | Cryptographic key management documented | Three-layer envelope documented in [GDPR Art. 17 — crypto-shred](gdpr-art-17-crypto-shred.md) | (docs) | — |
| 3.7.6 | Cryptographic keys cannot be substituted | Manifest signature binds DEK envelope to manifest; tampering surfaces as `verify.manifest_signature` | `pg_hardstorage verify ...` | `verify.manifest_signature` (on fail) |
| 3.7.7 | Split knowledge / dual control | n-of-m approval for `kms.shred` and other destructive ops | `pg_hardstorage approval request ...` | `approval.request`, `approval.approve` |

---

## Req. 9 — Restrict Physical Access

PCI DSS Req. 9 covers physical access to systems and
media. For cloud backup repos, the storage backend's
operator (AWS, Azure, GCP) attests via their own SOC 2 /
PCI Type II reports. `pg_hardstorage` contributes:

| Requirement | Description | Product feature | Command | Audit event |
| --- | --- | --- | --- | --- |
| 9.4.1 | Media inventory | `repo usage` enumerates every chunk + manifest in the repo | `pg_hardstorage repo usage` | (read-only) |
| 9.4.2 | Media classification | Data classification tags propagate to every backup | `pg_hardstorage classify set ...` | (manifest tag) |
| 9.4.3 | Media destruction documented | Crypto-shred + audit event is the documented destruction path | `pg_hardstorage kms shred ...` | `kms.shred` |
| 9.4.4 | Personnel destroying media | Audit chain records actor on every `kms.shred` | (automatic) | `kms.shred` records `actor` |

---

## Req. 10 — Log and Monitor

| Requirement | Description | Product feature | Command | Audit event |
| --- | --- | --- | --- | --- |
| 10.2.1 | Logs capture all access to system components | Hash-chained audit log + structured JSON logs | `pg_hardstorage audit search ...` | (every event) |
| 10.2.1.1 | Logs include user actions | `actor` field on every audit event | (automatic) | (every event) |
| 10.2.1.2 | Logs include privileged actions | n-of-m approval flow records `approval.request` and the gated op | `pg_hardstorage approval list ...` | `approval.*`, gated ops |
| 10.2.1.3 | Logs include access to audit logs | Read access (`audit search` / `audit verify-chain`) is non-mutating and is **not** itself recorded as an audit event; chain reads are governed at the RBAC layer | `pg_hardstorage audit search ...` | (no audit event) |
| 10.2.1.4 | Logs include invalid logical-access attempts | RBAC denials recorded as `auth.denied` | (automatic) | `auth.denied` |
| 10.2.1.5 | Logs include authentication mechanism changes | KEK rotation + key changes recorded | `pg_hardstorage kms rotate` | `kms.rotate` |
| 10.2.1.6 | Logs include init / stop of audit logs | Control-plane startup is logged as `control_plane.starting`; there is no discrete audit-chain stop event | (automatic) | `control_plane.starting` |
| 10.3 | Log records contain timestamps | RFC3339 UTC timestamp on every event | (automatic) | (every event) |
| 10.5 | Audit logs cannot be modified | Hash-chained Merkle log + WORM bucket support | `pg_hardstorage audit verify-chain` | `verify.audit_chain_broken` (on fail) |
| 10.5.4 | Logs of public-facing components retained | WORM retention via S3 Object Lock / Azure immutable blob | (config) | (WORM-locked on write) |
| 10.6 | Time synchronisation | UTC timestamps; NTP recommended in operator guide | (operator) | — |
| 10.7 | Investigate logs at least daily | Insider-threat scanner runs against audit log | `pg_hardstorage insider scan` | `insider.scan` |

---

## Req. 12.10 — Incident Response

| Requirement | Description | Product feature | Command | Audit event |
| --- | --- | --- | --- | --- |
| 12.10.1 | Incident response plan | Runbooks R1–R7 + structured `doctor` remediation | `pg_hardstorage doctor` | `doctor.suggested_fix` |
| 12.10.5 | Specific incidents (data leakage etc.) | Audit evidence bundle export for forensics | `pg_hardstorage audit export-bundle ...` | (read-only) |

---

## SAD (Sensitive Authentication Data) handling

PCI DSS prohibits storage of SAD post-authorization (full
track data, CVV, PIN). If `pg_hardstorage` is backing up a
database that legitimately processes SAD pre-authorization
(payment processor / acquirer), the standard backup-data
crypto controls above apply.

For databases that **must not retain SAD post-auth**, use
the source-side PII redaction plugin (or column-level
masking via the `redact` subcommand)
before chunks land in the repo. Crypto-shred is the
mitigation if SAD ends up backed up by mistake.

---

## WORM enforcement

PCI DSS Req. 10.5 (audit logs cannot be modified) is
satisfied by:

```yaml
deployments:
  pci-prod:
    repo: 's3://acme-pci-backups/?region=us-east-1'
    worm: true
    worm_retention: 365d   # PCI: 1 year minimum
```

The S3 Object Lock policy locks every committed
manifest + audit event for the configured retention. The
backend reports `worm.active = true` in the compliance
report.

---

## Generating the per-window assessment

```sh
pg_hardstorage compliance report \
    --repo s3://acme-pci-backups/ \
    --since 2026-01-01 --until 2026-04-01 \
    -o json \
    | jq '.result.body.controls.controls[]
          | select(.framework == "pci_dss")'
```

---

## QSA-ready evidence package

For PCI DSS audits, export an evidence bundle covering the
audit period plus the cosign signatures on the running
binary:

```sh
VERSION=1.0.10   # the release / image tag you're attesting

# 1. Audit chain bundle
pg_hardstorage audit export-bundle \
    --repo s3://acme-pci-backups/ \
    --since 2026-01-01T00:00:00Z \
    --until 2026-04-01T00:00:00Z \
    --include-anchors \
    --out ./qsa-q1-2026-audit.tar.gz

# 2. Compliance report (Markdown for the QSA)
pg_hardstorage compliance report \
    --repo s3://acme-pci-backups/ \
    --since 2026-01-01 --until 2026-04-01 \
    -o markdown \
    > ./qsa-q1-2026-controls.md

# 3. Build provenance attestation
cosign verify-attestation \
    --type slsaprovenance \
    "ghcr.io/cybertec-postgresql/pg_hardstorage:v${VERSION}" \
    --certificate-identity-regexp \
        "https://github.com/cybertec-postgresql/pg_hardstorage/.*" \
    --certificate-oidc-issuer \
        "https://token.actions.githubusercontent.com" \
    > ./qsa-q1-2026-build-provenance.json
```

---

## Further reading

- [SOC 2 control mapping](soc2-control-mapping.md) —
  cross-reference.
- [Audit evidence bundles](audit-evidence-bundles.md) — the
  10.5 evidence export.
- [GDPR Article 17 — crypto-shred](gdpr-art-17-crypto-shred.md)
  — Req. 3.6.1.4 / 9.4.3 destruction detail.
- [SLSA L3 provenance](slsa-l3-provenance.md) — supply
  chain integrity for Req. 6.3.
