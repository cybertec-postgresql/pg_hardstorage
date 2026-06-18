# backup/

The take-a-backup pipeline: stream BASE_BACKUP into a content-addressed chunk
store and commit a signed manifest.

## What lives here

Everything that runs between `pg_hardstorage backup` and the moment a manifest
is durable in the repo. Chunking (FastCDC), manifest authorship + signing,
retention policy, KEK custody, tar parsing of the BASE_BACKUP stream,
hold/legal-hold, undelete, verifybackup gate, attestation gate.

## End-to-end flow

`cli.backup` → `runner.Take` → `pg/basebackup.BASE_BACKUP` → `tarsink` →
`chunker.FastCDC` → `repo.CAS.Put` → `manifest.Sign` (Ed25519) →
`manifest_store.Commit`.

## Key files / subdirs

- `runner/` — orchestrator: opens the wire stream, drives the chunker, writes
  the manifest
- `chunker/` — FastCDC content-defined chunking (min/avg/max bounds)
- `manifest.go` / `manifest_store.go` — signed JSON manifest schema + durable
  commit
- `sign.go` — Ed25519 signer/verifier, PEM serialization (`SchemeEd25519`)
- `tarsink/` — reads the BASE_BACKUP tar stream and hands payload chunks to
  CAS
- `keystore/` — KEK custody (`kek.go`, `unwrap.go`, shred)
- `retention/` — `simple.go`, `count.go`, `gfs.go`, `policy.go` —
  chain-anchor aware
- `verifybackup/` — invokes `pg_verifybackup` against a staged restore
- `attestgate/` — attestation gate: refuses to ship a manifest without a valid
  signer
- `hold.go` / `rotate.go` — legal-hold lifecycle and active-manifest rotation
- `compare.go` — diff two manifests (for `backup compare`)
- `chunk_check.go` / `verify_envelopes.go` — manifest self-consistency checks

## Read next

- `../restore/README.md` — the inverse direction
- `../repo/README.md` — where chunks and manifests actually land
- `../pg/basebackup/` — the wire protocol this layer drives
- `../chain/` — full-chain graph utilities

## Don't put X here

- Wire-protocol code (PG message handling) — that's `internal/pg/`.
- CAS read/write internals — that's `internal/repo/`.
- CLI flag parsing — that's `internal/cli/backup.go`.
