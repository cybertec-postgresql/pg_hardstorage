---
title: Sink plugin contract
description: The output.Sink interface — asynchronous fan-out of events to external systems.
tags:
  - plugins
  - sinks
  - reference
---

# Sink plugin contract

A sink fans `output.Event` values out to an external
system: Slack, PagerDuty, Jira, syslog, OpenTelemetry,
ServiceNow, a generic webhook.  The active renderer
formats one stream of events for the operator's terminal;
sinks run **alongside** the renderer in their own
goroutines and consume the same event stream — so a long
restore narrates progress to stdout *and* posts a
checkpoint message to `#pg-backups` *and* opens a Jira
ticket if anything goes wrong.

!!! note "Reference implementations"
    - `internal/plugin/sink/slack/slack.go` — the
      canonical example; webhook POST, severity filter,
      airgap-aware.
    - `internal/plugin/sink/syslog/` — the RFC 5424
      lossless mapping (severity is direct).
    - `internal/plugin/sink/jira/` — async issue creation
      with deduplication keys.
    Read `slack.go` first — it's the most direct
    illustration of the Sink + SinkBuilder contract.

## Interface

```go
// internal/output/dispatcher.go

package output

type Sink interface {
    Name() string
    Open(ctx context.Context, cfg map[string]any) error
    Emit(ctx context.Context, ev *Event) error
    Close() error
}
```

## Lifecycle

```
   Register(plugin, builder)    ─ at init() time
              │
              ▼
   builder(SinkSpec)            ─ called once per configured sink at startup
              │
              ▼
   Open(ctx, cfg)               ─ called by the dispatcher before first Emit
              │
              ▼
   Emit(ctx, ev)                ─ called once per event, possibly concurrently
              │
              ▼
   Close()                      ─ called once at process exit
```

The dispatcher's `Close()` blocks until every in-flight
`Emit` returns — a `sync.WaitGroup` tracks them — so a
slow sink never observes a `Close` mid-`Emit`.  Sinks
that buffer or batch should flush on `Close`.

## Per-method contract

### `Name() string`

The operator's label for this configured sink — e.g.
`"ops-slack"`, `"audit-cef"`, `"pagerduty-primary"`.
Surfaces in events the dispatcher emits *about* this
sink (open errors, panic recovery).  Set from
`SinkSpec.Name`, NOT from the plugin's class name.

### `Open(ctx context.Context, cfg map[string]any) error`

Initialise — open HTTP connection pools, validate auth
tokens, perform any startup handshake.  In practice most
sinks treat `Open` as a no-op because validation happened
in the builder; it's reserved for future
auth-handshake-at-runtime patterns.

`cfg` is the same map passed to the builder; the
dispatcher passes it through unchanged so a sink that
defers expensive setup until first `Emit` has it
available.

### `Emit(ctx context.Context, ev *Event) error`

Send one event downstream.  `ev` is a fully-formed
`*output.Event` (schema, severity, component, op,
subject, body, suggestion, trace, generated_at).

**Filtering.**  Most sinks honour a `min_severity` config
key.  The pattern (from `slack.go`) is:

```go
if !ev.Severity.AtLeast(s.minSeverity) {
    return nil   // dropped, not failed
}
```

Note RFC 5424 semantics: lower numeric value = more
severe.  `SeverityError.AtLeast(SeverityWarning) == true`
because error is more severe than warning.  The
`Severity.AtLeast` helper hides the inversion so plugin
code reads naturally.

**Failure handling.**  Returning a non-nil error from
`Emit` is logged by the dispatcher but does NOT abort the
operation that produced the event.  A flaky Slack webhook
shouldn't fail a backup.  Implementations that need
delivery guarantees layer their own retry/queue.

**Per-call cost.**  `Emit` is on the hot path during
streaming operations.  HTTP-based sinks should reuse a
`*http.Client` across calls (set in the constructor), not
construct one per event.

### `Close() error`

Release resources.  Idempotent.  Should flush any
batched / queued events before returning.  The dispatcher
honours this by waiting for in-flight `Emit` calls before
calling `Close`.

## Severity model

```go
const (
    SeverityEmergency Severity = iota   // 0 - system unusable
    SeverityAlert                       // 1
    SeverityCritical                    // 2
    SeverityError                       // 3
    SeverityWarning                     // 4
    SeverityNotice                      // 5
    SeverityInfo                        // 6
    SeverityDebug                       // 7
)
```

RFC 5424.  Direct mapping for syslog and CEF; the JSON /
NDJSON renderers carry both the numeric and the string
form (`severity_name`).

## Configuration shape

A configured sink is declared in `pg_hardstorage.yaml`:

```yaml
sinks:
  - name: ops-slack
    plugin: slack
    config:
      webhook_url: https://hooks.slack.com/services/...
      channel: "#pg-backups"
      min_severity: warning
```

The host parses this into:

```go
type SinkSpec struct {
    Name   string         `yaml:"name"`
    Plugin string         `yaml:"plugin"`
    Config map[string]any `yaml:"config,omitempty"`
}
```

…and looks up `Plugin` in the registry.  `Name` and
`Plugin` are required.  `Config` is plugin-specific.

## Builder pattern

A sink registers a `SinkBuilder` — a function that turns
a `SinkSpec` into a ready-to-`Open` `Sink`:

```go
type SinkBuilder func(spec SinkSpec) (Sink, error)

func init() {
    output.DefaultSinkRegistry.Register("slack", NewFromSpec)
}

func NewFromSpec(spec output.SinkSpec) (output.Sink, error) {
    url, err := output.SinkConfigString(spec.Config, "webhook_url")
    if err != nil {
        return nil, err
    }
    if url == "" {
        return nil, errors.New("slack: config.webhook_url is required")
    }
    // ... validate, parse min_severity, construct ...
    return &Sink{...}, nil
}
```

**Validate at the builder, not at `Emit`.**  An invalid
config should fail at startup with a clear message, not
silently drop events at runtime.

`SinkConfigString` and `SinkConfigStringDefault` (in
`internal/output/sinkspec.go`) are the canonical
type-safe accessors for `cfg map[string]any` keys; use
them so type errors surface as
`"config key 'min_severity': expected string, got int"`
rather than panics.

## Error sentinels

```go
var ErrUnknownSinkPlugin = errors.New("output: unknown sink plugin")
```

Returned by `SinkRegistry.Build` when the spec's `Plugin`
field doesn't match any registered builder.

`SinkBuildError` wraps a builder failure with the
offending `SinkSpec`:

```go
type SinkBuildError struct {
    Spec SinkSpec
    Err  error
}
```

`SinkRegistry.BuildAll(specs)` collects per-spec failures
into a slice the caller renders as warning events — one
bad sink config doesn't block the rest.

## Registration

Double-registration **panics** (programmer error, not
runtime):

```go
func (r *SinkRegistry) Register(plugin string, builder SinkBuilder) {
    if _, ok := r.builders[plugin]; ok {
        panic(fmt.Sprintf("output: sink plugin %q already registered", plugin))
    }
    r.builders[plugin] = builder
}
```

Tier-2 sinks register their builder against
`output.DefaultSinkRegistry` after the discovery phase;
since Tier-1 sinks pre-register at `init()`, a Tier-2
sink that wants to override a Tier-1 must use a different
plugin name.

## Concurrency contract

| Operation | Concurrent calls allowed? |
| --- | --- |
| `Emit` from multiple goroutines | Yes — sinks MUST be goroutine-safe |
| `Open` / `Close` | Serial; host serializes |
| `Emit` while `Close` is in flight | NO — host's `WaitGroup` blocks `Close` |

## Airgap interaction

Sinks that talk to external services (Slack, Jira,
PagerDuty, …) MUST consult `airgap.Default().EndpointAllowed(url)`
in their constructor and refuse if the endpoint is
disallowed:

```go
if err := airgap.Default().EndpointAllowed(url); err != nil {
    return nil, fmt.Errorf("slack: %w", err)
}
```

The `airgap` package implements the routable-private-IP
allowlist that covers `PG_HARDSTORAGE_AIRGAPPED=1`
deployments.  Local sinks (`syslog` to a Unix socket,
file-based audit log) bypass this.

## What sinks MUST get right

1. **`Emit` doesn't block.**  Long emit calls back-
   pressure the producer.  Use a per-sink goroutine pool
   for any operation that takes more than ~100 ms.
2. **Failures don't propagate.**  A returning-non-nil
   `Emit` is logged but doesn't fail the operation that
   produced the event.
3. **`Close` flushes.**  Buffered events emit before
   `Close` returns.  The dispatcher's `WaitGroup` won't
   block on a sink that lost its events on shutdown.
4. **Severity filtering happens at `Emit`.**  Filtering at
   the dispatcher would force the dispatcher to know about
   per-sink config.

## Tier-2 mapping

The Tier-2 gRPC contract (see
`proto/plugin/v1/plugin.proto` `service SinkPlugin`)
mirrors the Go interface with one structural change: the
`Event` message uses `google.protobuf.Struct` for
`subject`, `body`, and `suggestion` so future event
schema additions don't require a proto bump.  The host
marshals each field through the canonical JSON
representation before sending.

## Further reading

- Output event schema: `reference/output-event-schema.md`
  (auto-generated).
- The dispatcher: `internal/output/dispatcher.go`.
- Audit-event schema (a related but separate stream the
  audit subsystem emits): `reference/audit-event-schema.md`.
