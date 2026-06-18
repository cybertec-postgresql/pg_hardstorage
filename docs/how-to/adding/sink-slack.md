---
title: Add a Slack sink
description: Wire pg_hardstorage events to a Slack incoming webhook
              with severity filtering.
tags:
  - sinks
  - slack
  - notifications
---

# Add a Slack sink

> The Slack sink posts each event to an incoming webhook. One CLI
> call adds the sink; the dispatcher fans events out
> asynchronously, so a slow webhook never stalls a backup.

## What you need

- A Slack incoming-webhook URL
  (`https://hooks.slack.com/services/T.../B.../...`). Create one
  via *Apps → Incoming WebHooks* in your workspace; pin it to
  the channel that should receive backup events.
- (Optional) A bot username for the messages — the webhook's
  default is fine.

## Steps

### 1. Add the sink

```bash
# RUNNABLE
pg_hardstorage notify add slack \
    --name ops-slack \
    --set webhook_url=https://hooks.slack.com/services/T000/B000/XXX \
    --min-severity warning
```

```console
sink "ops-slack" added (plugin=slack)
```

The CLI runs the same builder the agent uses at start-up, so a
malformed URL or missing key is rejected before it lands in
`pg_hardstorage.yaml`.

### 2. Verify

```bash
pg_hardstorage notify list
```

```console
NAME       PLUGIN  MIN_SEVERITY  COMPONENTS
ops-slack  slack   warning       *
```

### 3. Smoke-test (optional)

Trigger any low-severity event the agent surfaces — e.g. a
verify run — and confirm the message appears in the channel:

```bash
pg_hardstorage verify db1 latest --repo file:///srv/pg_hardstorage/repo
```

## Configuration reference

```yaml
sinks:
  - name: ops-slack
    plugin: slack
    config:
      webhook_url: https://hooks.slack.com/services/T.../B.../...
      channel: "#pg-backups"            # optional; overrides webhook default
      username: "pg_hardstorage"        # optional bot username
    filter:
      min_severity: warning             # default: notice
      components: ["backup", "wal.stream", "verify", "kms"]
```

| Key | Default | Notes |
| --- | --- | --- |
| `webhook_url` | required | Treated as a secret; logs redact it. |
| `channel` | webhook default | Override the default channel pinned to the webhook. |
| `username` | `pg_hardstorage` | Bot username shown in the message header. |
| `min_severity` | `notice` | RFC 5424; the sink emits when severity ≤ floor. |
| `components` | all | Allowlist of components (`backup`, `wal.stream`, `verify`, `kms`, `audit`, …). |

## Severity floor

RFC 5424 ladder: `emergency=0 < alert=1 < critical=2 < error=3
< warning=4 < notice=5 < info=6 < debug=7`. The sink emits
when an event's severity number is **less than or equal to**
the floor — a `warning` floor sends warning + error + critical +
alert + emergency, but not notice / info / debug.

Pick `error` for "wake people up," `warning` for "interesting,"
`notice` for "all the things." `debug` is firehose territory.

## Air-gap interaction

The webhook URL is checked against `airgap.allowlist` at
sink-add time. Under `PG_HARDSTORAGE_AIRGAPPED=1` the
`hooks.slack.com` host needs to either resolve to an RFC1918
address (forward proxy) or appear in the allowlist explicitly.

## Troubleshooting

**`slack: config.webhook_url is required`** — the `--set` was
empty or misspelled. Re-run with the full URL.

**`webhook_url forbidden by airgap policy`** — strict mode,
no allowlist entry. Add the host (or the proxy that fronts it)
to `airgap.allowlist`.

**Messages never appear** — confirm the webhook is still active;
a Slack admin who deletes the app instantly invalidates every
URL it produced. The sink doesn't probe at start-up (a Slack
ping costs a request per agent reboot); a 4xx on emit logs
once-per-minute and is visible in the agent's journal.

**Rate-limited** — Slack throttles incoming webhooks at
~1 request/second per webhook. The sink uses a simple HTTP
POST; for very chatty fleets, raise `min_severity` or split into
per-team webhooks.

## Next steps

- [Add a Jira sink](sink-jira.md) for ticketed escalation
- [Add a PagerDuty sink](sink-pagerduty.md) for paging
- [Sink dispatcher reference](../../operations/operator-guide.md#10-sinks)
- [`notify` CLI reference](../../reference/cli/pg_hardstorage_notify.md)
