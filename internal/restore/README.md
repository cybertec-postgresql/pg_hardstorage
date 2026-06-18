# restore/

The inverse of `backup/`: materialise a Postgres data directory from a manifest,
optionally through a chain of incrementals, optionally with PITR.

## What lives here

The `Restore` entrypoint, plan/execute split, preflight gates, plain vs chain
dispatch, encryption glue, postverify, naturaltime parsing, PITR GUC emission,
tablespace remap, redaction.

## Key files / subdirs

- `restore.go` — `Restore` entrypoint, plain restore path
- `plan.go` — materialised plan: which manifest, which chain, which target
- `recovery.go` — emits `recovery_target_*` GUCs into `postgresql.auto.conf`
  for PITR
- `timetarget.go` — resolves `--to` into an LSN/time/xid
- `naturaltime/` — parses `"2h ago"`, `"yesterday 14:00 Europe/Vienna"`,
  `+HH:MM` offsets
- `latest.go` — resolve `--latest` against the manifest store
- `gapcheck.go` — refuse if required WAL segments are missing from archive
- `verify.go` / `postverify/` — pre- and post-restore `pg_verifybackup`
  invocation
- `encryption_glue.go` — KEK resolver callback handed to the chunk reader
- `combine/` — wraps `pg_combinebackup` for chain restore (manifest.Type
  `incremental_lsn`)
- `tablespace_remap.go` — `--tablespace-mapping` rewriter
- `checkpoint.go` — resumable-restore checkpoint state
- `safejoin_test.go` — defence against path-traversal in tar entries
- `walfetchcmd/` — `restore_command` shim that fetches WAL on demand
- `redact/` — secret-scrubbing for restore-time logs

## Chain restore

When `manifest.Type == "incremental_lsn"`, `Restore` walks back through the
anchor chain, materialises each link into a scratch dir, then calls
`combine.Run` (`pg_combinebackup`) to fold them into the final target.

## Preflight gates

Target must not exist, must not be a Postgres datadir, must not have a running
postmaster. Each gate emits a structured `output.Error` — never a panic.

## Read next

- `../backup/README.md` — produces what this consumes
- `../wal/README.md` — supplies the segments PITR needs
- `../pg/basebackup/README.md` if present — the manifest schema mirrors this

## Don't put X here

- Manifest authoring — that's `internal/backup/manifest.go`.
- WAL gap discovery logic — that's `internal/wal/gapstate/` (we only consume
  it).
- Cobra command bodies — that's `internal/cli/restore*.go`.
