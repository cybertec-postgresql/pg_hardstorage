# integrity/

Continuous attestation: periodic integrity Runs re-verify manifest signatures
and chunk presence to catch bit-rot before it surfaces at restore time.

## What lives here

The unit of evidence is a signed `Run`. Each Run records the subset of the repo
scanned, the strategy used, and the result. Strategies trade scan cost for
assurance:

- `manifests-only` — fastest; signatures only, no chunk I/O
- `presence` (default) — chunk `Stat` for every referenced chunk
- `content-sample:N` — re-fetch N% of chunks and recompute plaintext SHA-256
- `content-full` — re-fetch every chunk

A failed Run identifies the bad chunk and (when a healthy replica exists) the
auto-heal path that `internal/repo/heal.go` will take.

## Key files

- `integrity.go` — `Run`, `Strategy`, `Verify`, `Sign`, storage layout
- `integrity_test.go` — strategy semantics, signature verification,
  recorded-evidence shape

## Read next

- `../threshold/README.md` — a Run body is a natural attestation subject
  (multi-party "the repo was intact at T")
- `../repo/README.md` (`heal.go`) — auto-heal from a healthy replica when a
  Run fails
- `../audit/README.md` — every Run is recorded in the audit chain
- `../README.md` — parent index

## Don't put X here

- Backup-time signing — that's `internal/backup/sign.go`.
- Chunk-level CAS reads — that's `internal/repo/cas.go`.
