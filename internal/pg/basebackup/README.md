# pg/basebackup/

The `BASE_BACKUP` wire-protocol implementation: speak the replication-protocol
command, consume the `CopyOut` stream, and surface tar payloads + manifest to
the backup pipeline.

## What lives here

The narrow PG-side adapter that turns the BASE_BACKUP replication command into a
Go iterator of tar bytes + a parsed `backup_manifest`. It handles the
full-backup protocol on every supported PG version and the **incremental** flow
added in PG 17 (UPLOAD_MANIFEST + boolean `INCREMENTAL` option). Higher layers
(`internal/backup`) consume what this package emits; this package never touches
CAS, encryption, or signing.

## Key files

- `basebackup.go` — `Run(ctx, conn, opts) (*Stream, error)`; option
  marshalling; CopyOut framing; manifest hand-off
- `basebackup_test.go` — protocol-level table tests with recorded fixtures
- `integration_test.go` — `//go:build integration` against a real PG via
  `internal/pg/testkit`

## Protocol notes

- Full backup: `BASE_BACKUP [LABEL ...] [PROGRESS] [FAST] [WAL] [MANIFEST 'yes']
  [MANIFEST_CHECKSUMS 'SHA256']`.
- Incremental (PG 17+): first send `UPLOAD_MANIFEST` with the prior backup's
  manifest, then `BASE_BACKUP ... INCREMENTAL`. The server returns only changed
  blocks.
- Tablespaces stream as separate tar payloads in the same CopyOut.
- Errors come back as `ErrorResponse` mid-stream; we propagate without
  swallowing.

## Read next

- `../replication/README.md` — slot lifecycle used alongside BASE_BACKUP for
  WAL continuity
- `../../backup/README.md` — the orchestrator that calls this package
- `../../wal/README.md` — WAL collection that pairs with each base
- `../../restore/README.md` — the inverse direction

## Don't put X here

- Chunking, encryption, manifest signing — those are `internal/backup/`.
- WAL streaming — that's `internal/pg/replication/` + `internal/pg/walsink/`.
- CLI flag plumbing — `internal/cli/backup.go`.
