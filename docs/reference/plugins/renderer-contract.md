---
title: Renderer plugin contract
description: The output.Renderer interface — synchronous formatting of Result and Event into bytes.
tags:
  - plugins
  - renderers
  - reference
---

# Renderer plugin contract

A renderer takes typed values — `*output.Result` for one-
shot commands, `*output.Event` for streaming commands —
and writes bytes to an `io.Writer`.  Exactly **one**
renderer is active per CLI invocation, picked by
`--output` / env / TTY auto-detection at startup.

This is the **synchronous, command-scoped** output tier.
Fan-out to external systems (Slack, syslog, PagerDuty)
goes through [Sinks](sink-contract.md), which run
alongside the active renderer and consume the same event
stream concurrently.

!!! note "Reference implementations"
    - `internal/plugin/renderer/text/text.go` — default
      human-readable renderer; opt-in `TextWriter`
      interface lets payloads format themselves.
    - `internal/plugin/renderer/ndjson/ndjson.go` — the
      streaming renderer (one JSON document per line, no
      indentation, per-event flush).
    - `internal/plugin/renderer/json/json.go` — pretty-
      printed JSON for one-shot commands.
    Read all three before writing your own; together they
    show the spectrum of single-event vs streaming
    behaviour.

## Interface

```go
// internal/output/renderer.go

package output

type Renderer interface {
    Name() string
    RenderResult(w io.Writer, r *Result) error
    RenderEvent(w io.Writer, e *Event) error
    SupportsTTY() bool
    Close() error
}
```

## Lifecycle

```
   New() (or constructor with options)   ─ at startup, after CLI flags parsed
              │
              ▼
   RenderResult OR RenderEvent (many)    ─ the dispatcher serializes calls
              │
              ▼
   Close()                                ─ once at end of CLI invocation
```

The dispatcher serializes every call through a mutex, so
**implementations may assume single-threaded access**
and don't need their own locking.  This is intentional —
renderers are often stateful (column-aligned text tables,
JSON Encoder reuse, ASCII-progress-bar repaint state) and
forcing them to be goroutine-safe would multiply
complexity for no gain.

## Per-method contract

### `Name() string`

Lowercase canonical name (`"text"`, `"json"`, `"ndjson"`,
`"junit"`, `"yaml"`, …).  Stable across versions; matched
case-sensitively against `--output` / `PG_HS_OUTPUT`.

### `RenderResult(w io.Writer, r *Result) error`

Writes a one-shot `Result`.  Called **exactly once** per
non-streaming command (`status`, `version`, `inspect`,
`verify`, `kms inspect`).  Implementations should write
a trailing newline so terminal sessions are tidy.

`Result` shape (`internal/output/event.go`):

```go
type Result struct {
    Schema      string    `json:"schema"`        // "pg_hardstorage.v1"
    Command     string    `json:"command"`
    GeneratedAt time.Time `json:"generated_at"`
    Result      any       `json:"result,omitempty"`   // success body
    Error       *Error    `json:"error,omitempty"`    // failure body
}
```

Either `Result` or `Error` is set, never both.  The text
renderer's pattern:

```go
func (r *Renderer) RenderResult(w io.Writer, res *output.Result) error {
    if res == nil { return nil }
    if res.IsError() {
        return r.renderError(w, res.Error)
    }
    return r.renderBody(w, res.Result)
}
```

### `RenderEvent(w io.Writer, e *Event) error`

Writes one streaming event.  Called **many times** per
streaming command (`backup`, `restore`, `verify --stream`,
`wal stream`, log tails).  Each invocation should write
**exactly one logical record**:

- Line-oriented renderers: one `\n`-terminated line.
- Paragraph-oriented (text): one paragraph + blank line.
- Document-oriented (junit, yaml): well, it's complicated
  for those — see the existing impls.

**Critical: do NOT internally wrap the writer in a
`bufio.Writer` that delays output.**  Streaming consumers
(`pg_hardstorage backup --output ndjson | jq`) rely on
per-event flush so progress is visible in real time.
The `json.Encoder` flushes on each `Encode`; that's the
pattern the ndjson renderer uses.

`Event` shape:

```go
type Event struct {
    Schema       string       `json:"schema"`
    Severity     Severity     `json:"severity"`
    SeverityName string       `json:"severity_name"`
    Component    string       `json:"component,omitempty"`
    Op           string       `json:"op,omitempty"`
    Subject      Subject      `json:"subject,omitzero"`
    Body         any          `json:"body,omitempty"`
    Suggestion   *Suggestion  `json:"suggestion,omitempty"`
    Trace        TraceContext `json:"trace,omitzero"`
    GeneratedAt  time.Time    `json:"generated_at"`
}
```

### `SupportsTTY() bool`

Reports whether this renderer is appropriate for an
interactive terminal.  Used by TTY auto-detection:
`--output text` is forced to `--output ndjson` when stdout
is not a TTY (and vice-versa) unless the operator
overrides.

| Renderer | `SupportsTTY` |
| --- | --- |
| `text` | `true` |
| `markdown` | `true` |
| `json` (pretty) | `true` (single-shot) |
| `ndjson` | `false` |
| `csv`, `yaml`, `junit`, `tap`, `html`, `pdf` | `false` |

### `Close() error`

Releases renderer-side resources.  Called once at the end
of the CLI invocation.  Most renderers have nothing to
release; this hook exists for future renderers that hold
file handles (the html / pdf renderers may emit a
trailing footer).

## Streaming vs single-event

The Renderer contract supports both modes through the
two methods.  In practice each renderer leans toward one:

| Renderer | Streaming behaviour | Single-event behaviour |
| --- | --- | --- |
| `text` | Paragraph per event, blank-line separated | Pretty multi-line block |
| `json` | One pretty doc per event (verbose) | One pretty doc total |
| `ndjson` | One compact line per event (the default for streaming) | One long line |
| `junit` | Aggregates events into a `<testsuite>` tree, emits at `Close` | One `<testcase>` |
| `tap` | Per-event `ok` / `not ok` line | Single `1..1` plan + line |
| `csv` | Header + row per event | Header + single row |
| `pdf`, `html` | Buffered through `Close`; emits one document | One document |

The CLI's TTY auto-detection picks `text` for human
operators and `ndjson` for pipes.  Operators can override
either via `--output <name>` or `PG_HS_OUTPUT=<name>`.

## The TextWriter opt-in

The text renderer supports an opt-in interface that lets
payload types format themselves:

```go
type TextWriter interface {
    WriteText(w io.Writer) error
}
```

If `Result.Result` (or `Event.Body`) implements
`TextWriter`, the text renderer calls `WriteText` and
trusts the output.  Otherwise it falls back to indented
JSON.  Implementations should NOT include a trailing
newline — the renderer adds one.

This pattern keeps domain-specific formatting (status
tables, version blocks, doctor reports) close to the
domain code rather than ballooning the renderer with
per-command rendering logic.

## Registration

Renderers do NOT self-register against a default registry
the way sinks and storage plugins do.  The dispatcher's
constructor takes one explicit `Renderer`:

```go
func NewDispatcher(renderer Renderer, out, err io.Writer) *Dispatcher
```

…and the CLI's startup wiring chooses which one to
construct based on `--output` / TTY auto-detection.  See
`cmd/pg_hardstorage/main.go` for the resolver function.

The reasoning: exactly one renderer is active per
invocation, so a registry / lookup-by-name pattern would
be ceremony for no benefit.  If you ship a Tier-1
renderer, add a case to the resolver alongside the
existing ones.

Tier-2 renderers register against the (forthcoming)
`output.DefaultRendererRegistry` and are looked up by
`Name()`; the proto for that path is at
`proto/plugin/v1/plugin.proto` `service RendererPlugin`.

## Concurrency contract

**Renderers may assume single-threaded access.**  The
dispatcher's mutex serializes every call.  Goroutine-
safety is NOT a requirement; in fact the text renderer's
ASCII-progress-bar state machine assumes the opposite.

Sinks (which run concurrently) handle the
goroutine-safety story for fan-out separately —
[Sink contract](sink-contract.md).

## What renderers MUST get right

1. **One logical record per `RenderEvent` call.**  No
   internal buffering across events.
2. **Per-event flush for streaming renderers.**  The
   `json.Encoder.Encode` pattern (one Write per record)
   is canonical.
3. **`SupportsTTY` doesn't lie.**  False positives leak
   ANSI escape codes into log files; false negatives
   give the operator a degraded experience.
4. **`Close` is idempotent.**  The dispatcher MAY call
   `Close` after a fatal error AND in the normal exit
   path.

## Further reading

- Output event schema: `reference/output-event-schema.md`
  (auto-generated).
- The dispatcher: `internal/output/dispatcher.go`.
- The Severity model:
  `internal/output/severity.go` — RFC 5424.
- Sinks (the asynchronous tier):
  [Sink contract](sink-contract.md).
