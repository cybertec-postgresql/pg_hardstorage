---
title: Add a PagerDuty sink
description: Wire pg_hardstorage events to PagerDuty Events API
              v2 with deterministic dedup keys.
tags:
  - sinks
  - pagerduty
  - alerting
---

# Add a PagerDuty sink

> The PagerDuty sink fires events to the Events API v2. Each
> event uses a deterministic `dedup_key` derived from the
> failure's identity tuple, so the same logical failure firing
> repeatedly resolves to **one** PD incident.

## What you need

- A PagerDuty service.
- An Events API v2 integration on that service. Create it from
  *Service → Integrations → Add Integration → Events API v2*.
  Copy the routing key (32 hex chars).

## Steps

### 1. Add the sink

```bash
pg_hardstorage notify add pagerduty \
    --name ops-pd \
    --set routing_key=<32-hex-routing-key> \
    --set source=pg_hardstorage@db1 \
    --min-severity error
```

```console
sink "ops-pd" added (plugin=pagerduty)
```

### 2. Verify

```bash
pg_hardstorage notify list
```

### 3. Smoke-test

Run a failing operation; confirm the incident appears in
PagerDuty under the configured service.

## Configuration reference

```yaml
sinks:
  - name: ops-pd
    plugin: pagerduty
    config:
      routing_key: "<32-hex-routing-key>"
      source: "pg_hardstorage@db1"     # default: "pg_hardstorage"
      client: "pg_hardstorage"         # appears in the incident
      client_url: "https://runbooks.acme.com/pg-hardstorage"
    filter:
      min_severity: error              # default: error
```

| Key | Default | Notes |
| --- | --- | --- |
| `routing_key` | required | 32-hex Events API v2 integration key. |
| `source` | `pg_hardstorage` | Free-form; appears as the alert's source. Pair with the deployment name. |
| `client` | empty | Optional client name on the incident. |
| `client_url` | empty | Optional URL on the incident — runbook link. |
| `min_severity` | `error` | PD is for waking people; the floor is deliberately strict. |

## Severity mapping

| pg_hardstorage severity | PagerDuty severity |
| --- | --- |
| `emergency`, `alert`, `critical` | `critical` |
| `error` | `error` |
| `warning` | `warning` |
| `notice`, `info`, `debug` | `info` |

PagerDuty maps `info` events to the alert log without paging.

## Dedup model

The sink derives `dedup_key = sha256(component + op + deployment + backup_id)`
and includes it on every PD `trigger`. Same logical failure
re-firing → same `dedup_key` → same incident. Operators who
silence "wal lag on db1" once don't get re-paged for the same
condition every agent tick.

`v0.1` ships only the `trigger` action. `acknowledge` and
`resolve` correlate to "this event resolves that incident,"
which lands when the audit slice can correlate audit-events to
incident IDs.

## Air-gap interaction

PD's events endpoint is hard-coded to
`https://events.pagerduty.com/v2/enqueue` — there's no on-prem
PD. Under `PG_HARDSTORAGE_AIRGAPPED=1`, the host needs to
appear in `airgap.allowlist` (or the proxy that fronts it).

## Troubleshooting

**`pagerduty: config.routing_key is required`** — the `--set`
was empty or misspelled.

**Test event never arrives** — `min_severity: error` (the
default) discards `notice` / `info`. Either raise the test
event's severity or temporarily set `min_severity: notice`.

**Incident keeps re-firing despite ack** — PD requires the
`resolve` action to clear an incident; until pg_hardstorage
emits `resolve` (v0.5), let PD auto-resolve via its inactivity
timer or close manually.

## Next steps

- [Add a Slack sink](sink-slack.md) for non-paging chat
- [Add a Jira sink](sink-jira.md) for ticketed follow-up
- [`notify` CLI reference](../../reference/cli/pg_hardstorage_notify.md)
