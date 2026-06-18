# partial/ (v0.5)

Table-level / partial restore: inspect a manifest for a specific relation, and
extract heap files for that relation only.

**Today:** `partial inspect` reports manifest entries for a `pg_class`
relfilenode; `partial restore` extracts the matching heap files into a target
directory. Includes a safe-join layer so a malicious manifest can't
path-traverse out of the target.

**Coming (v0.5):** the auto-spin-up-sandbox path — start a disposable PG
against the extracted heap files and run `pg_dump` on the chosen tables so the
operator gets logical SQL output, not raw heap pages.

## Why heap-files-out first, sandbox-pg-dump later

Heap-file extraction is decoupled from PG availability — works against a cold
repo, no sandbox PG needed. The sandbox path adds operator convenience but
doesn't change what's recoverable. Shipping heap extraction first keeps the
surface honest about what works today.

## Key files

- `relfilenode.go` — resolve `(schema, table)` to a `pg_class.relfilenode`
  (libpq URI or manifest scan)
- `restore.go` — extract heap files, safe-join + decryption, `RestoreResult`
  (`pg_hardstorage.partial.restore.v1`)
- `sandbox/` — disposable PG sandbox harness for the v0.5 pg_dump path
  (`sandbox.go`)
- `relfilenode_test.go` / `restore_test.go` / `safejoin_test.go` — relfilenode
  resolution + safe-join coverage
- `tablelevel_integration_test.go` — end-to-end against a fixture manifest

## Read next

- `../restore/README.md` — full-restore plumbing; partial reuses chunk
  decryption + manifest walks
- `../verify/sandbox/` — the disposable PG harness referenced by
  `partial/sandbox/`
- `../README.md` — parent index

## Don't put X here

- Logical-replication output — that's `internal/logical/`.
- Full-database restore — that's `internal/restore/`.
