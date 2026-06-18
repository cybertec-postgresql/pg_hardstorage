---
title: Compliance
description: Control mappings and evidence-bundle workflows.
---

# Compliance

`pg_hardstorage` is built so the auditor's first questions
have ready answers. This section maps product controls to
the major regulatory frameworks and documents the
evidence-bundle workflow.

The canonical source for any framework matrix below is the
output of:

```sh
pg_hardstorage compliance report --repo <url> -o markdown
```

The report walks the audit chain, encryption coverage,
verification activity, and approval / hold lifecycle, and
emits a verdict table per framework — pass / fail /
not-applicable per control, with one-line evidence and a
remediation pointer for each fail.

---

## Mechanisms

- [GDPR Article 17 — crypto-shred](gdpr-art-17-crypto-shred.md)
  — the per-tenant KEK design and the `kms shred` flow.
- [Audit evidence bundles](audit-evidence-bundles.md) —
  signed forensic packages exported via
  `audit export-bundle` / `audit verify-bundle`.
- [Data residency pinning](data-residency-pinning.md) —
  per-deployment region constraints.
- [SLSA L3 build provenance](slsa-l3-provenance.md) —
  cosign signatures + reproducible builds verified weekly.

## Framework mappings

- [SOC 2 control mapping](soc2-control-mapping.md) — AICPA
  Trust Services Criteria (CC, A, PI, C, P).
- [ISO 27001 control mapping](iso-27001-control-mapping.md)
  — ISO/IEC 27001:2022 Annex A.
- [HIPAA mapping](hipaa.md) — Technical Safeguards (45 CFR
  §164.308 / §164.312).
- [PCI DSS mapping](pci-dss.md) — Req. 3, 9, 10 (v4.0).
- [FedRAMP mapping](fedramp.md) — NIST SP 800-53 Moderate
  baseline.

---

## Where the mapping data lives in the codebase

The framework-to-control assertions are encoded in
`internal/compliance/controls.go`. Every entry below is
backed by an audit event class produced by the
corresponding feature path. The matrix is a single source
of truth — if the binary's behaviour disagrees with the
docs, the binary wins, and an issue should be filed.

The four cross-cutting building blocks behind every
mapping are:

1. **The hash-chained audit log**
   ([`internal/audit`](https://github.com/cybertec-postgresql/pg_hardstorage/tree/main/internal/audit))
   — what produces every "who did what" record.
2. **The three-layer envelope encryption** — what
   produces the "data is protected at rest" guarantees.
3. **The signed manifest + chunk integrity loop** — what
   produces the "data is unmodified" guarantees.
4. **The n-of-m approval workflow** + JIT tokens — what
   produces the "destructive ops are authorised" guarantees.

---

## Further reading

- [Operator guide: encryption + KMS](../operations/operator-guide.md#7-encryption-kms)
  — the operational view of the three-layer envelope.
- [Operator guide: audit log](../operations/operator-guide.md#8-audit-log)
  — running, searching, and verifying the chain.
- [Architecture tour](../explanation/architecture-tour.md)
  — how the four building blocks compose.
