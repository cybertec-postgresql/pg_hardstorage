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
  - **Byte-identical exit codes.**  See below — the
    zero / non-zero split is honoured, the specific
    numbers are not.

## Exit codes

A wrapper script that tests **"did the tool exit non-zero?"**
behaves the same against a shim as against the real tool.  A
script that branches on a *specific* number does not, and this
is the sharpest edge in the whole compat surface — so it is
worth stating plainly rather than leaving to be discovered.

Real pgBackRest carries a rich per-failure-class numbering and
prints it in-band (`ERROR: [037]: ...`): 31 for expire-class
errors, 37 for a stanza lookup, 55 for a missing config file,
and so on.  The shim does not reproduce those.  It returns:

| code | meaning |
|------|---------|
| 0    | success |
| 1    | the native CLI failed, or the shim could not dispatch |
| 2    | a refusal — unknown verb, or a flag whose effect the shim will not silently drop |

Real Barman uses 0 / 1 / 2 (2 = usage), which is close; the
notable divergence there is that an unknown command is 1 in the
shim and 2 in real barman.

Two deliberate exceptions, where the exit code carries meaning
the caller cannot recover any other way:

  - **`barman check --nagios`** returns Nagios' own codes —
    0 OK, 1 WARNING, 2 CRITICAL, 3 UNKNOWN — because that is
    the entire point of the flag.
  - **`barman-cloud-wal-restore`** returns **1** only for a
    segment that is genuinely absent, and **126** for any other
    failure.  PostgreSQL reads every plain non-zero exit from a
    `restore_command` as "end of archive" and PROMOTES; 126 lands
    on `wait_result_is_any_signal`'s signal-ish branch so
    recovery aborts instead.  Never "fix" a wrapper by
    collapsing that to 1.

If you have automation that branches on pgBackRest's numbers,
convert it to branch on the structured output instead: every
native command accepts `--output json` and reports a stable
`error.code` (`notfound.backup`, `conflict.backup_in_progress`,
`source_corruption.data_checksum`, ...) under the
`pg_hardstorage.v1` schema, with a 24-month compatibility
commitment.

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
