# compat/ — drop-in replacement shims for legacy backup tools

This directory carries the v1.1+ compatibility shims that
let operators running `pgbackrest`, `barman`, or `wal-g`
today switch to `pg_hardstorage` without rewriting their
cron jobs, `archive_command` settings, or monitoring
scripts.

## What's here

  - **`pgbackrest/`** — shim that mimics the `pgbackrest`
    CLI surface; built as `bin/pg-hardstorage-pgbackrest`.
  - **`barman/`** — shim that mimics the `barman` CLI
    surface; built as `bin/pg-hardstorage-barman`.
  - **`walg/`** — shim that mimics the `wal-g` CLI
    surface; built as `bin/pg-hardstorage-walg`.

Each subdirectory is **self-contained** — its own command
tree, flag/env mapping, output formatter, and config
translator.  No shared code today; we'll refactor common
utilities into `compat/shared/` if a fourth shim arrives.

## What's NOT here (deliberate non-goals)

  - **Reading existing pgBackRest / Barman / WAL-G
    repository formats.** The repo formats are binary-
    tagged and undocumented externally; reverse-
    engineering is multi-quarter work for a feature that
    the dual-write + retention-drain migration pattern
    doesn't need.  Old repos stay on the original tool
    until their retention expires; new backups go to a
    fresh pg_hardstorage repo.
  - **Byte-identical output.**  Semantic equivalence is
    enough for `grep`-based monitoring.
  - **Every flag / env var.**  ~12 most-cited pgBackRest
    flags + ~10 Barman + the WAL-G env-var convention;
    the rest refuse with a remediation pointing at the
    native pg_hardstorage equivalent.

## Architecture

Each shim binary embeds `internal/cli`'s Cobra command
tree and dispatches via synthetic `os.Args` rather than
forking a separate `pg_hardstorage` process.  This:

  - keeps coupling at the CLI surface (the public contract),
    not the internal Go API
  - is one process — no fork overhead per invocation
  - tests can intercept by capturing the rendered Args
  - new flags on the native CLI light up automatically

A typical shim verb file looks like:

```go
func runPgbackrestBackup(args pgbackrestBackupArgs) error {
    nativeArgs := []string{
        "backup",
        args.stanza,
        "--pg-connection", buildPGConn(args),
        "--repo", buildRepoURL(args),
    }
    if args.backupType == "incr" {
        nativeArgs = append(nativeArgs, "--incremental-from", "latest")
    }
    if args.backupType == "diff" {
        return refuseWithRemediation("--type=diff",
            "use --type=incr (PG 17 page-level incremental); see docs/how-to/migration/from-pgbackrest.md")
    }
    root := cli.NewRoot()
    root.SetArgs(nativeArgs)
    return root.Execute()
}
```

## Migration story

End-to-end pattern: see
[`docs/how-to/migration/from-pgbackrest.md`](../docs/how-to/migration/from-pgbackrest.md),
[`docs/how-to/migration/from-barman.md`](../docs/how-to/migration/from-barman.md),
and
[`docs/how-to/migration/from-walg.md`](../docs/how-to/migration/from-walg.md).
TL;DR — translate config once, drop the shim into PATH,
existing scripts run unchanged, retention-drain the old
tool over its window.
