# cli/

The cobra command tree. Every `pg_hardstorage <verb>` invocation lands in a
`newReal*Cmd()` function in this package.

## What lives here

~60 verbs and their flag wiring, the output dispatcher (renderer × sink
fan-out), arg validators, YAML config IO, the dispatch table, and the
cheatsheet/cmdtree generators consumed by docs.

## Key files / subdirs

- `root.go` — root command, global flag plumbing, `stub()` helper (currently
  called once for `repo compact`)
- `dispatch.go` — the verb → implementation table
- `args_error.go` — typed argument-validation errors (structured exit codes)
- `output_flags.go` — `--output`, `--quiet`, `--no-color`, `--no-tty`,
  `--explain` plumbing
- `configio.go` — YAML config load/merge with env override
- `cmdtree/` — introspection of the cobra tree (used by `dump_cmd_tree.go` and
  docs)
- `redact.go` — secret scrubber applied to every emitted event
- `refuse_root.go` — refuse to run as UID 0 unless explicitly allowed
- `plugins_register.go` — register every built-in plugin into its plugin
  registry
- `onboard_helpers.go` — interactive onboarding helpers for `init`
- `runbook_templates.go` — embedded runbook bodies for `runbook` verbs
- `backup*.go`, `restore*.go`, `repo*.go`, `wal*.go`, `agent*.go`, ... — one
  file per verb family

## Conventions

- Every verb defines a `newReal<Verb>Cmd()` returning a `*cobra.Command`.
- No verb writes to stdout directly; output flows through `internal/output`.
- Long-form help text is rendered from `i18n/` catalogs where present.

## Read next

- `../output/README.md` if it exists — every command emits through it
- `../config/README.md` if it exists — YAML schema this package loads
- `../../docs/reference/` — user-facing flag reference generated from this
  tree

## Don't put X here

- Business logic — verbs should orchestrate other packages, not implement
  them.
- Direct PG protocol code — call `internal/pg/`.
- Manifest or CAS reads — call `internal/backup/` or `internal/repo/`.
