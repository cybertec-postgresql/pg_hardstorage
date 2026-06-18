<!-- AUTO-GEN candidate: reflect over output.Event / output.Result / output.Error struct tags; per docs/DOC_PLAN.md auto-generation map. -->
---
title: Output event schema
description: The wire format for streaming events, command results, and structured errors.
tags:
  - reference
  - output
  - events
---

# Output event schema

`pg_hardstorage` emits two document shapes: `Event` (the
streaming-output unit — one progress tick, one log line,
one notification, one audit record) and `Result` (the
one-shot command-output unit).  Both ride the schema
string `pg_hardstorage.v1`; 24-month back-compat applies.

Source: [`internal/output/event.go`](https://github.com/cybertec-postgresql/pg_hardstorage/blob/main/internal/output/event.go).

## `Event`

| Field (JSON) | Go type | Required | Notes |
| --- | --- | --- | --- |
| `schema` | string | yes | `pg_hardstorage.v1` |
| `severity` | int8 | yes | RFC 5424 numeric severity (0 = emergency, 7 = debug) |
| `severity_name` | string | yes | Canonical lowercase name; mirrors `severity` for human consumers |
| `component` | string | no | Subsystem name (`backup`, `restore`, `wal`, `kms`, `repo`, …) |
| `op` | string | no | Operation within the component (`backup.start`, `kms.unwrap`, …) |
| `subject` | object | no (omitzero) | See [`Subject`](#subject) |
| `body` | any | no | Free-form payload; per-op shape |
| `suggestion` | object | no | See [`Suggestion`](#suggestion) |
| `trace` | object | no (omitzero) | See [`TraceContext`](#tracecontext) |
| `generated_at` | RFC3339 timestamp | yes | UTC; set by `NewEvent` |

NDJSON renderer emits one Event per line; the text
renderer emits one Event per paragraph.

## `Severity`

RFC 5424.  Lower numeric value = more severe.

| Value | Name | Meaning |
| --- | --- | --- |
| 0 | `emergency` | system unusable |
| 1 | `alert` | action required immediately |
| 2 | `critical` | critical condition |
| 3 | `error` | error condition |
| 4 | `warning` | warning condition |
| 5 | `notice` | normal but significant |
| 6 | `info` | informational |
| 7 | `debug` | debug-level |

`Severity.AtLeast(threshold)` returns true when the event
is at least as severe as the threshold (i.e. has a
**lower** numeric value).

`MarshalText` / `UnmarshalText` accept the canonical names
plus short aliases (`emerg`, `crit`, `err`, `warn`,
`informational`).  Unknown names are an error — config
loaders surface typos rather than silently downgrade.

## `Subject`

The "who/what does this concern?" tuple.  Every field is
optional; the renderer renders only what's set.

| Field | Type | Notes |
| --- | --- | --- |
| `tenant` | string | Multi-tenant slot |
| `deployment` | string | Logical deployment name |
| `backup_id` | string | When the event concerns a specific backup |
| `timeline` | uint32 | PG timeline ID |
| `lsn` | string | LSN |

## `Suggestion`

The remediation triple.  Every field optional.

| Field | Type | Notes |
| --- | --- | --- |
| `human` | string | What we print to a TTY |
| `command` | string | Literal shell string the operator can copy or pipe |
| `doc_url` | string | Link to a runbook or how-to |

When triaging, the suggestion is the load-bearing field.
Renderers always surface it; sinks should preserve it.

## `TraceContext`

W3C-style trace identifiers.  Populated when the agent is
part of a traced operation.

| Field | Type | Notes |
| --- | --- | --- |
| `trace_id` | string | W3C `traceparent` trace ID |
| `span_id` | string | Active span ID |

## `Result`

The one-shot command-output unit.  A `pg_hardstorage status`
invocation produces exactly one `Result` wrapping the actual
payload.  Either `result` or `error` is set, never both.

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `schema` | string | yes | `pg_hardstorage.v1` |
| `command` | string | yes | Canonical command path (e.g. `backup`, `verify`, `kms shred`) |
| `generated_at` | RFC3339 timestamp | yes | UTC |
| `result` | any | when success | Per-command body shape |
| `error` | object | when failure | See [`Error`](#error) |

The JSON renderer emits the entire `Result`; the text
renderer prints either the body or the error message.

## `Error`

The structured-error type that flows through the output
system.  Implements `error`; commands can
`return &output.Error{…}` from cobra's `RunE`.  The
dispatcher walks the wrapped chain (`errors.As`) to extract
the structured form for JSON / NDJSON / sink emission, and
to derive the [exit code](exit-codes.md).

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `code` | string | yes | Dotted lowercase code; first segment is the [namespace](error-codes.md) |
| `message` | string | yes | Operator-readable summary |
| `severity` | int8 (omitempty) | no | Defaults to `error` (3); upgrade to `critical` / downgrade to `warning` via `WithSeverity` |
| `subject` | object | no (omitzero) | Same shape as Event.Subject |
| `suggestion` | object | no | Same shape as Event.Suggestion |
| `cause` | (not serialised) | no | Wrapped error for `errors.Is` / `errors.As` chains |

`*Error.Error()` returns `"<code>: <message>"` — the cause
is not in the string.  JSON consumers see `code` and
`message` as separate fields; the cause chain is for
typed `errors.Is` matching, not for display.

`output.ToError(err)` is the single place where ad-hoc
`error` values enter the structured world: structured
errors pass through unchanged; others are wrapped with
`code: "internal"` at severity `error`.

## Sentinel errors

| Sentinel | Meaning |
| --- | --- |
| `output.ErrUsage` | "the user invoked the CLI wrong"; the dispatcher maps this to exit code 2 |

Cobra-internal errors (unknown flag, missing arg) are
wrapped with `ErrUsage` so the CLI can detect them
uniformly.

## See also

- [Exit codes](exit-codes.md) — namespace → exit-code
  mapping.
- [Error codes](error-codes.md) — the catalogue of
  `code` values, grouped by namespace.
- [Plugins → Renderer contract](plugins/renderer-contract.md)
  — how renderers consume Events.
- [Plugins → Sink contract](plugins/sink-contract.md) —
  how sinks fan-out Events to external systems.
