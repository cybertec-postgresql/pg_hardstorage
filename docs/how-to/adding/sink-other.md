---
title: Other sinks (CEF, Datadog, Email, Teams, Discord, …)
description: Pointer page covering the rest of the built-in sink
              plugins — CEF, Datadog, Email, OpenTelemetry events,
              ServiceNow, Splunk HEC, Microsoft Teams, Discord,
              Opsgenie.
tags:
  - sinks
  - integration
---

# Other sinks

> The five flagship sinks have dedicated pages
> ([Slack](sink-slack.md), [Jira](sink-jira.md),
> [PagerDuty](sink-pagerduty.md), [Webhook](sink-webhook.md),
> [Syslog](sink-syslog.md)). The rest live here, with the
> minimal config skeleton each one expects. Every sink is
> wired through `pg_hardstorage notify add`.

## Common pattern

```bash
pg_hardstorage notify add <plugin> \
    --name <id> \
    --set key=value \
    --set key2=value2 \
    --min-severity <level>
```

The CLI runs the same builder the agent uses at start-up, so
typo'd keys / missing required fields are rejected before
landing in `pg_hardstorage.yaml`. See the
[`notify add` CLI reference](../../reference/cli/pg_hardstorage_notify_add.md)
for the full flag list.

---

## CEF (`cef`)

ArcSight Common Event Format over TCP / TLS. For SIEMs that
prefer CEF over RFC 5424's JSON message body.

```yaml
sinks:
  - name: prod-cef
    plugin: cef
    config:
      protocol: tls
      address: siem.example.com:514
      vendor: pg_hardstorage
      product: pg_hardstorage
      version: "1"
```

CEF severity is rendered as 0-10 from the RFC 5424 ladder.

## Datadog (`datadog`)

Posts events to Datadog's `/api/v1/events` endpoint with the
Datadog API key.

```yaml
sinks:
  - name: dd
    plugin: datadog
    config:
      api_key: <DATADOG_API_KEY>
      site: datadoghq.com         # or datadoghq.eu, ddog-gov.com
      tags: ["service:pg_hardstorage", "env:prod"]
```

`min_severity: warning` is the typical pick — Datadog is a
chat-room peer, not a pager.

## Email (`email`)

Plain SMTP with three TLS modes (`starttls`, `implicit`,
`none`) and three auth modes (`plain`, `login`, `none`).

```yaml
sinks:
  - name: ops-email
    plugin: email
    config:
      smtp_host: smtp.example.com
      smtp_port: 587
      tls_mode: starttls
      auth_mode: plain
      username: pg-hardstorage
      password_secret: kms-secret://ops/smtp-password
      from: backups@example.com
      to: ["dba@example.com"]
      cc: ["ops@example.com"]
    filter:
      min_severity: error
```

`min_severity: error` is the right default; nobody wants email
on every WAL keepalive.

## Microsoft Teams (`teams`)

Posts adaptive cards to a Teams Incoming Webhook (Power
Automate workflow URL).

```yaml
sinks:
  - name: ops-teams
    plugin: teams
    config:
      webhook_url: https://acme.webhook.office.com/...
```

Same severity / dedupe model as the Slack sink. Use one or the
other; running both produces twice the noise.

## Discord (`discord`)

Posts to a Discord webhook URL. Identical shape to Slack /
Teams — useful for community / OSS projects that run their ops
in Discord.

```yaml
sinks:
  - name: discord
    plugin: discord
    config:
      webhook_url: https://discord.com/api/webhooks/...
```

## Opsgenie (`opsgenie`)

Atlassian's pager. Parallel to PagerDuty: deterministic dedup
key, `min_severity: error` default.

```yaml
sinks:
  - name: og
    plugin: opsgenie
    config:
      api_key: <OPSGENIE_API_KEY>
      region: us              # us | eu
      team: dba-on-call       # routes to the team's escalation
```

## ServiceNow (`servicenow`)

Creates ServiceNow incidents. Same `dedupe_by_subject` /
`always_new` posture as the Jira sink.

```yaml
sinks:
  - name: sn
    plugin: servicenow
    config:
      base_url: https://acme.service-now.com
      username: svc-pg-hardstorage
      password: <secret>
      table: incident
      assignment_group: dba-team
```

## Splunk HEC (`splunkhec`)

HTTP Event Collector for Splunk. POSTs to the HEC endpoint
with the per-token auth header.

```yaml
sinks:
  - name: splunk
    plugin: splunkhec
    config:
      url: https://splunk.example.com:8088/services/collector
      token: <HEC_TOKEN>
      sourcetype: pg_hardstorage:event
      index: pg_hardstorage
```

## OpenTelemetry events (`otelevents`)

Emits events as OTLP/HTTP log records to an OpenTelemetry
collector. Pair with the agent's `--otel-endpoint` for the
trace path; this sink covers the structured-log path.

```yaml
sinks:
  - name: otel
    plugin: otelevents
    config:
      endpoint: http://otel-collector:4318
      service_name: pg_hardstorage
      service_namespace: dba
```

## Severity model recap

Every sink applies the same RFC 5424 floor (`min_severity:
<level>`). The dispatcher emits when the event's severity number
is **less than or equal to** the floor — `warning` floor sends
warning + error + critical + alert + emergency.

A sink that panics is recovered; siblings still receive the
event; a diagnostic line goes to stderr. See the
[sink dispatcher reference](../../operations/operator-guide.md#10-sinks)
for the full guarantees.

## Next steps

- [Add a Slack sink](sink-slack.md)
- [Add a syslog sink](sink-syslog.md) — most-common SIEM feed
- [Sink dispatcher reference](../../operations/operator-guide.md#10-sinks)
- [`notify` CLI reference](../../reference/cli/pg_hardstorage_notify.md)
