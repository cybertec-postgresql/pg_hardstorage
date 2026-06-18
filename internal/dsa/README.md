# dsa/

GDPR Data Subject Access (DSA) helper: given a subject ID + tenant, locate which
backups contain that subject's data and emit a signed disclosure report.

## What lives here

pg_hardstorage cannot peek into encrypted chunk content (and shouldn't — the
keystore is outside our trust boundary). The natural unit of GDPR compliance is
the *tenant*: each tenant has its own KEK, and `kms shred` operates at tenant
granularity. So DSA reports walk manifests filtered by tenant and enumerate
every affected backup + its `KEKRef`.

The operator supplies an opaque `subject_id` (UUID, hashed email, internal user
ID, ...) and asserts the tenant boundary. We SHA-256-hash the raw `subject_id`
before persisting, so the report is safe to retain alongside the audit chain.
Output schema: `pg_hardstorage.dsa.report.v1`, persisted at
`dsa/reports/<id>.json`.

## Key files

- `dsa.go` — `Request`, `Report`, subject hashing, manifest walk + tenant
  filter, signing
- `dsa_test.go` — report shape, subject-ID hashing, signature verification

## Read next

- `../compliance/README.md` — control mappings; the DSA workflow contributes
  evidence to Article 15 / 17 controls
- `../audit/README.md` — every DSA request is hash-chained
- `../../docs/compliance/gdpr-art-17-crypto-shred.md` — the user-facing
  pairing of `kms shred` + DSA
- `../README.md` — parent index

## Don't put X here

- KEK shred — that's `internal/kms/` + the `kms shred` CLI; DSA only *locates*
  affected backups.
- Subject-to-tenant mapping — operator responsibility; pg_hardstorage never
  sees raw subject data.
