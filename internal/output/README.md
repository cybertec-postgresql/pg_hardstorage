# output/

The typed event spine: every CLI command emits results through one `Event` →
`Dispatcher` → renderer + sink fan-out, never through `fmt.Println`.

## What lives here

The contract that lets `pg_hardstorage backup` print a human-readable line on
stdout, ship a JSON record to syslog, post a Slack card, and emit a Datadog
event from a single emission site. The renderer + sink plugin surfaces (in
`internal/plugin/renderer` and `internal/plugin/sink`) are wired through the
`Dispatcher` defined here.

## Key files

- `event.go` — `Event{Subject, Severity, Code, Title, Detail, Fields, Time}`;
  `Subject` identifies the cluster/backup/object the event is about
- `severity.go` — RFC 5424 levels: `emerg`, `alert`, `crit`, `err`, `warning`,
  `notice`, `info`, `debug`; ordered, comparable, parseable
- `dispatcher.go` — fan-out engine; concurrent renderer/sink invocation with
  per-sink timeouts and failure isolation
- `renderer.go` — `Renderer` interface (`Render(io.Writer, Event)` +
  `RenderStream`)
- `sinkspec.go` — declarative sink filter: severity floor, component
  allow/deny, rate limit, dedupe window
- `exitcode.go` — Severity → POSIX exit-code mapping (`err`+ → non-zero)

## Read next

- `../plugin/renderer/README.md` — the 11 shipped renderers
- `../plugin/sink/README.md` — the 15 shipped sinks
- `../cli/` — call sites that construct events
- `../audit/` — separate, append-only journal; not the same as a sink

## Severity → exit code

`emerg`/`alert`/`crit`/`err` → non-zero exit. `warning` keeps a successful
exit but flips a warning bit the wrapper script can read.
`notice`/`info`/`debug` are silent on exit code.

## Don't put X here

- Domain logic (no backup, restore, WAL code).
- Per-sink transport (HTTP clients, SMTP) — that's a sink plugin.
- Free-text formatting — render through a `Renderer`, never `fmt.Fprintf`
  directly.
