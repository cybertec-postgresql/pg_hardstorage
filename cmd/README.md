# cmd

Go `main` packages — one subdirectory per shipped binary. Every executable
produced by this repo has its entry point here; business logic lives under
`../internal/`.

## What lives here

Each subdirectory is a `package main` with a single `main.go` that wires flags,
signals, and config, then calls into `../internal/`. Adding a new binary means
adding a new subdirectory plus an entry in `../Makefile` and `../packaging/`.
Keep `main.go` thin — no policy, no I/O loops, no tests.

## Key files / subdirs

- `pg_hardstorage/` — the primary CLI / server binary (full feature set)
- `pg_hardstorage_simple/` — minimal "just take a backup" binary for embedded
  / sidecar use
- `pg_hardstorage_testkit/` — orchestrator for the scenario runner under
  `../test/scenarios/`
- `pg-hardstorage-barman/` — drop-in Barman shim (translates Barman argv to
  our control plane)
- `pg-hardstorage-barman-wal-archive/` — WAL archive command counterpart to
  the Barman shim
- `pg-hardstorage-pgbackrest/` — drop-in pgBackRest shim
- `pg-hardstorage-walg/` — drop-in WAL-G shim
- `pg-hardstorage-compat/` — shared compat helper used by the three shim
  binaries above
- `docsgen/` — generates `../docs/reference/cli/`, `../man/man1/`, and
  `../completions/*/`
- `doctest/` — extracts and executes fenced code blocks from `../docs/` (CI
  gate)

## Read next

- `../internal/cli/README.md` — where the CLI command tree actually lives
- `../docs/reference/cli/index.md` — the rendered CLI reference these binaries
  expose
- `../packaging/README.md` — how these binaries are mapped to OS packages

## Don't put X here

- Test files (`*_test.go`) — exercise the logic in `../internal/`, not the
  `main` wrapper.
- Library code reused across binaries — promote it to `../internal/`.
- Generated artefacts — `docsgen` writes into `../docs/`, `../man/`,
  `../completions/`, never back into `cmd/`.
