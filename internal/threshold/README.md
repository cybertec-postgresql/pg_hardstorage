# threshold/

k-of-n attestations: a quorum of trusted signers vouches for a backup manifest,
audit anchor, or KEK rotation.

## What lives here

A roster names `n` signers and a threshold `k`. An attestation is a header + a
set of independent ed25519 signatures over the same subject hash. We use
multi-signature aggregation (not Shamir secret-sharing): every signature is
independently verifiable, no share-distribution ceremony is needed, and rosters
can rotate without invalidating prior attestations (each attestation pins the
roster hash it was signed under).

## Key files

- `threshold.go` — roster + attestation types, signing, verify
- `quorum.go` — k-of-n quorum arithmetic and finalisation
- `*_mutation_*.go` — mutation-audit shim that perturbs the off-by-one
  boundary
- `threshold_test.go` — roster lifecycle + signature verification +
  roster-hash pinning

Storage layout under the repo: `threshold/rosters/<id>.json`,
`threshold/attestations/<kind>/<id>/header.json`,
`threshold/attestations/<kind>/<id>/sigs/<fpr>.json`.

## Read next

- `../approval/README.md` — n-of-m approvals for destructive ops (similar
  shape, different lifecycle)
- `../audit/README.md` — anchors and audit bundles are common attestation
  subjects
- `../integrity/README.md` — integrity Run bodies pair with threshold
  attestations
- `../README.md` — parent index

## Don't put X here

- KEK custody — that's `internal/kms/` / `internal/backup/keystore/`.
- Operator-key generation — uses the same ed25519 keypair as manifest signing
  (`internal/backup/sign.go`).
