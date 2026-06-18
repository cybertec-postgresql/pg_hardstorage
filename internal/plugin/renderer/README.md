# plugin/renderer/

The renderer tier: turn an `output.Event` into bytes — human-readable,
machine-readable, or report-grade.

## What lives here

Implementations of the `Renderer` interface. One Event in, one stream of bytes
out, the same data shaped for an ops engineer on stdout, a JSON ingester, a PDF
compliance binder, or a CI test harness. Renderers are stateless across calls;
streaming renderers (`RenderStream`) handle long-running commands that emit many
events.

## Renderer interface

`Render(io.Writer, Event) error` — one-shot. `RenderStream(io.Writer)
StreamWriter` — open a stream, write N events, close. `ContentType() string`
— for HTTP / sink negotiation.

## Plugins

| Name | Scope | Status |
| --- | --- | --- |
| `text` | Human-readable colored text; default for TTY stdout | real |
| `json` | Pretty-printed JSON; one event per call | real |
| `ndjson` | Newline-delimited JSON; default for streamed output | real |
| `yaml` | YAML; alternative to JSON for configs / diffs | real |
| `template` | Go `text/template` with user-provided template | real |
| `csv` | RFC 4180 CSV; flattens `Event.Fields` to columns | real |
| `html` | Standalone HTML page with embedded CSS | real |
| `markdown` | GitHub-flavoured Markdown; good for issue bodies | real |
| `pdf` | PDF report-grade output for compliance binders | real |
| `tap` | Test Anything Protocol v13; for CI gates | real |
| `junit` | JUnit XML; consumed by CI test reporters | real |

## Read next

- `../../output/README.md` — the `Event` type they format
- `../sink/README.md` — many sinks delegate body formatting here
- `docs/reference/output-formats.md` — user-facing format reference

## Don't put X here

- Delivery — renderers write to an `io.Writer`; transport is sink-side.
- Filtering — that's `SinkSpec`.
- Stateful aggregation — renderers are per-event; aggregation belongs
  upstream.
