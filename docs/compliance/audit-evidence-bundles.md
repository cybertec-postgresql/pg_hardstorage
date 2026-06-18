---
title: Audit evidence bundles
description: Signed forensic packages exported from the audit chain via audit export-bundle / verify-bundle.
tags:
  - audit
  - evidence
  - forensics
---

# Audit evidence bundles

The audit chain is the canonical record of every operator
action against a `pg_hardstorage` repository — backups,
restores, KMS rotations, holds, approvals, KEK shred. The
chain itself is hash-linked and tamper-evident; an auditor
verifying it walks the events and confirms each event's
hash + chain link.

For external audit / forensic / compliance review, the
chain is exported as a **signed evidence bundle** —
a gzipped-tar with the events, a chain proof, the
operator's signing public key, and a detached Ed25519
signature.

---

## Lead with the command

### Export

```sh
pg_hardstorage audit export-bundle \
    --repo s3://acme-backups/ \
    --since 2026-04-01T00:00:00Z \
    --until 2026-05-01T00:00:00Z \
    --include-anchors \
    --operator "ops@acme" \
    -o ./acme-april-2026.tar.gz
```

### Verify

```sh
pg_hardstorage audit verify-bundle ./acme-april-2026.tar.gz
```

The verifier extracts the tarball, reconstructs the
canonical signing input, and verifies the Ed25519
signature against the embedded public key. A non-zero
exit means the bundle has been tampered with after
export.

---

## Bundle layout

```
acme-april-2026.tar.gz
├── bundle.json         # manifest: window, filters, file list, key fingerprint
├── events.ndjson       # one audit event per line, in commit order
├── anchors.ndjson      # anchor history (when --include-anchors set)
├── chain_proof.json    # head pointer + segment edge events
├── public_key.pem      # operator's Ed25519 signing public key (PEM)
├── README.md           # verifier instructions inside the tarball
└── signature.sig       # detached Ed25519 signature
```

### `bundle.json` schema

```json
{
  "schema": "pg_hardstorage.audit.evidence_bundle.v1",
  "generated_at": "2026-05-01T09:00:00Z",
  "operator": "ops@acme",
  "source_url": "s3://acme-backups/",
  "since": "2026-04-01T00:00:00Z",
  "until": "2026-05-01T00:00:00Z",
  "filters": {},
  "event_count": 4127,
  "anchor_count": 3,
  "head_hash": "f7e2c3...",
  "head_sequence": 4126,
  "public_key_fingerprint": "9a1b2c3d4e5f6789",
  "signed_files": ["events.ndjson", "anchors.ndjson",
                    "chain_proof.json", "public_key.pem",
                    "README.md", "bundle.json"],
  "signature_algorithm": "ed25519"
}
```

`signed_files` records the file order; the auditor
reproduces the canonical signing input by concatenating
each file's bytes in this exact order (per the format
documented in
[`internal/audit/bundle.go`](https://github.com/cybertec-postgresql/pg_hardstorage/tree/main/internal/audit/bundle.go)).

---

## How the signature works

We sign the SHA-256-prefixed concatenation of file bytes
— **not** the tar bytes — so re-archiving with a different
tar tool (different timestamps, header padding) doesn't
invalidate the signature.

The canonical input format is deliberately simple +
tar-tool-agnostic:

```
<file count>          8-byte big-endian uint64
for each file:
  <name length>       4-byte BE uint32
  <name>              UTF-8 bytes
  <body length>       8-byte BE uint64
  <body>              raw bytes
```

`signature.sig` itself is excluded — it can't sign itself.

---

## Verifying outside the binary

For auditors who don't trust running the same binary
they're auditing, the verifier is replicable from any
language with Ed25519 + SHA-256:

1. Untar `acme-april-2026.tar.gz`.
2. Read `bundle.json`; note `signed_files` order and the
   public key fingerprint.
3. Load `public_key.pem` (PKIX-encoded Ed25519 public key,
   block type `PG_HARDSTORAGE ED25519 PUBLIC KEY`).
4. Concatenate per the canonical-input format above, in
   `signed_files` order.
5. Verify `signature.sig` against the canonical bytes with
   the public key.
6. Walk `events.ndjson` and check each event's `prev_hash`
   matches the previous event's `hash`.

The Go reference implementation is
`audit.VerifyBundle(io.Reader)` — same signature contract
as `audit verify-bundle`. Future v0.5+: TypeScript +
Python verifiers in the public registry.

---

## Chain proof

`chain_proof.json` captures the edge of the chain at bundle
time so an auditor can verify the included events form a
contiguous segment without fetching the rest of the repo:

```json
{
  "schema": "pg_hardstorage.audit.chain_proof.v1",
  "head_hash": "f7e2c3...",
  "head_sequence": 4126,
  "first_event": {
    "id": "01H7K8...",
    "sequence": 3893,
    "hash": "8a91...",
    "prev_hash": "5e44..."
  },
  "last_event": {
    "id": "01H7L2...",
    "sequence": 4126,
    "hash": "f7e2c3...",
    "prev_hash": "1c8d..."
  }
}
```

The auditor walks `events.ndjson` and asserts each event's
`prev_hash` matches the prior event's `hash`. The first
event's `prev_hash` is checked against the bundle's
contextual prior — either the chain's `GenesisHash`
(`0000…`) for a from-zero export, or a separately-anchored
predecessor for a windowed export.

---

## Anchors

When `--include-anchors` is set, the bundle includes every
anchor whose `AnchoredAt` falls in the window. An anchor
is a periodic write of the chain head's hash + sequence
to a transparency log. The bundle's anchor list lets the
auditor verify that the chain head at every recorded anchor
matches the rebuilt chain.

---

## Filters

`audit export-bundle` accepts the same filters as
`audit search`:

| Flag | Effect |
| --- | --- |
| `--action backup.create` | Exact match on event action |
| `--action-prefix backup.` | All events in the `backup.*` namespace |
| `--actor ops@acme` | Specific actor |
| `--tenant acme-prod` | Specific tenant |
| `--deployment db1` | Specific deployment |
| `--backup-id <id>` | All events touching this backup |
| `--since` / `--until` | Time window (RFC3339) |

The applied filters are recorded in `bundle.json.filters`
so the auditor knows what was excluded — a partial bundle
with `--action-prefix kms.` excludes everything else, but
the filter recording makes that explicit.

---

## When to export

| Trigger | Recommended cadence |
| --- | --- |
| Quarterly compliance review | Every 90 days |
| Incident response | Once at incident close, with `--since` covering the incident window |
| Annual audit | Yearly bundle covering the audit period |
| Subject access request | Per-subject filtered bundle (`--actor` or `--tenant`) |
| Customer offboarding | Final bundle for the customer's tenant before `kms shred` |

For the final-bundle-before-shred case: export FIRST,
record the bundle's SHA-256 separately, THEN run shred.
The audit event for `kms.shred` lands AFTER the bundle was
exported; chain it via a follow-up bundle.

---

## What is NOT in the bundle

- **Backup data.** The bundle is audit-chain-only; no
  manifest, no chunk, no WAL.
- **Encryption keys.** No KEK material, no DEK material,
  no private signing key. Only the **public** Ed25519 key
  for verification.
- **PII.** Audit events don't carry row-level data; the
  bundle's events name backups by ID and deployments by
  name only.

---

## Compliance mapping

| Framework | Control | Bundle covers |
| --- | --- | --- |
| SOC 2 | CC7.2, CC7.3 | Tamper-evident logging, system event monitoring |
| ISO 27001 | A.8.15 | Logging |
| HIPAA | §164.312(b) | Audit controls |
| PCI DSS | Req 10.5 | Audit trail integrity |
| FedRAMP | AU-9, AU-11 | Audit info protection, retention |

---

## Further reading

- [Operator guide: audit log](../operations/operator-guide.md#8-audit-log)
  — the operational view.
- [Source: `internal/audit/bundle.go`](https://github.com/cybertec-postgresql/pg_hardstorage/tree/main/internal/audit/bundle.go)
  — the canonical implementation.
- [SLSA L3 provenance](slsa-l3-provenance.md) — the binary
  that produced the bundle has its own attestation chain.
