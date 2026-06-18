# repo/

The content-addressed store that backs every backup, every WAL segment, and
every manifest.

## What lives here

CAS read/write (`cas.go`), the on-disk path scheme (`layout.go`), garbage
collection (`gc.go`), bit-rot scrub + auto-heal (`heal.go`), cross-region
replication (`replicate.go`), WORM enforcement, ACLs, and the `HSREPO` marker
file that identifies a directory as a repo.

## Path scheme

- `HSREPO` — magic file at the repo root (`layout.go: HSREPOFilename`); JSON
  metadata, schema version
- `chunks/sha256/aa/bb/aabb<rest-of-hex>.chk` — chunks, two-byte fan-out (see
  `cas.go:59`)
- `manifests/<deployment>/...` — signed manifests
- WAL segments and gap-state files under their own prefixes (owned by
  `internal/wal/` + `internal/pg/walsink`)

## Key files / subdirs

- `init.go` — initialise an empty repo (race-safe via `Put(IfNotExists=true)`
  on `HSREPO`)
- `layout.go` — path constants, `Metadata` schema, schema version
- `cas.go` — `Put` / `Get` / `Stat` / `Delete`; fan-out path computation
- `chunkkey.go` — chunk-key derivation (input to encryption + dedup)
- `hash.go` — canonical SHA-256 wiring used by every layer above
- `gc.go` — mark-sweep collector: walks manifests, marks reachable chunks,
  deletes the rest
- `heal.go` — bit-rot scrub: re-hashes chunks, repairs from a replica if
  available
- `replicate.go` / `replicate_verify.go` — async cross-region copy with
  end-to-end verify
- `worm.go` — WORM (write-once read-many) enforcement; refuses
  overwrite/delete in WORM mode
- `setmode.go` — flip repo mode (e.g. normal → WORM); rewrites `HSREPO`
- `walprune.go` — prune WAL segments older than retention horizon
- `wipe.go` — destructive `repo wipe` implementation (gated by approval)
- `acl/` — per-deployment ACL records
- `bundle/` — air-gap export/import bundles
- `casdefault/` — default CAS implementation registration

## Read next

- `../backup/README.md` — primary producer of chunks + manifests
- `../wal/README.md` — produces WAL segments stored here
- `../plugin/storage/README.md` if present — the underlying object-store
  adapters

## Don't put X here

- Postgres-protocol code — `internal/pg/`.
- Manifest signing — `internal/backup/sign.go`.
- KMS calls — `internal/kms/` (with adapters in `internal/plugin/kms/`).
