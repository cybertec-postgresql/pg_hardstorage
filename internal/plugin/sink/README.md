# plugin/sink/

The sink tier: outbound delivery of `output.Event`s — chat, ticketing, paging,
SIEM, observability.

## What lives here

Implementations of the `Sink` interface. Each sink owns its transport (HTTP,
SMTP, syslog, OpenTelemetry) and declares a filter (severity floor, component
allow/deny, rate limit, dedupe window) that the dispatcher honours before
invoking `Emit`. Sinks are sandboxed: a failing or hanging sink can never block
the emitter or break another sink.

## Sink interface

`Emit(ctx, Event) error`, `Filter() SinkSpec`, `Name() string`, `Close()`.
Configured per-sink in the YAML `sinks:` block.

## Plugins

| Name | Scope | Status |
| --- | --- | --- |
| `slack` | Slack incoming webhooks + Block Kit cards | real |
| `pagerduty` | PagerDuty Events API v2 (trigger / ack / resolve) | real |
| `webhook` | Generic HMAC-signed JSON POST | real |
| `email` | SMTP with STARTTLS / SMTPS, templated body | real |
| `syslog` | RFC 5424 over UDP / TCP / TLS | real |
| `cef` | ArcSight Common Event Format over syslog | real |
| `splunkhec` | Splunk HTTP Event Collector | real |
| `datadog` | Datadog Events API + log intake | real |
| `jira` | Jira Cloud / Server: create + transition issues | real |
| `opsgenie` | Opsgenie Alert API | real |
| `servicenow` | ServiceNow incident table REST API | real |
| `teams` | Microsoft Teams incoming webhooks (Adaptive Cards) | real |
| `discord` | Discord webhooks | real |
| `otelevents` | OpenTelemetry logs / events OTLP exporter | real |
| `dispatch` | Capturing in-memory fixture for tests | test-only |

## Read next

- `../../output/README.md` — the `Event`, `Severity`, `Dispatcher` they
  consume
- `../renderer/README.md` — many sinks delegate body formatting to a renderer
- `docs/how-to/configure-sinks.md` — per-sink configuration cookbook

## Don't put X here

- Storage of events — sinks are write-through delivery; persistence is
  `internal/audit`.
- Domain logic — sinks must work without knowing what `backup.failed` *means*.
- Inbound webhooks — that's the server / API layer, not a sink.
