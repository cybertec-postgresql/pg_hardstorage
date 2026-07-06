---
title: Add a webhook sink
description: POST every event as JSON to a custom HTTP endpoint —
              the universal sink for in-house tooling.
tags:
  - sinks
  - webhook
  - integration
---

# Add a webhook sink

> The webhook sink is the universal escape hatch: every event is
> POSTed as JSON to a configured URL. Use it for in-house
> tooling, custom routers, or any system that doesn't have a
> dedicated sink plugin yet.

## What you need

- A reachable HTTPS endpoint that accepts `POST application/json`.
- Optionally, a bearer token or shared secret for an
  `Authorization` header.

## Steps

### 1. Add the sink

```bash
pg_hardstorage notify add webhook \
    --name ops-webhook \
    --set url=https://ops.example.com/hooks/pg-hardstorage \
    --set 'auth_header=Bearer eyJhbGciOi...' \
    --min-severity warning
```

```console
✓ Sink added — ops-webhook (plugin webhook)
```

### 2. Verify

```bash
pg_hardstorage notify list
```

### 3. Smoke-test

Pipe a verify run, watch the receiver log. The body shape is
the standard `pg_hardstorage.v1` event JSON (same as the
[output schema](../../reference/api/index.md)).

## Configuration reference

```yaml
sinks:
  - name: ops-webhook
    plugin: webhook
    config:
      url: https://ops.example.com/hooks/pg-hardstorage
      method: POST                       # default POST; PUT also accepted
      auth_header: "Bearer eyJ..."       # optional Authorization
      content_type: application/json     # default
      timeout: 10s                       # connect + write
    filter:
      min_severity: warning              # default: notice
```

| Key | Default | Notes |
| --- | --- | --- |
| `url` | required | Full HTTPS URL; the path is whatever your endpoint expects. |
| `method` | `POST` | `PUT` also accepted; everything else is rejected at builder time. |
| `auth_header` | empty | Sent verbatim as `Authorization`. |
| `content_type` | `application/json` | Override only for niche endpoints. |
| `timeout` | `10s` | Total request timeout (connect + write + response). |
| `min_severity` | `notice` | RFC 5424 floor. |

## Body shape

The body is the same Event JSON the dispatcher emits to other
sinks ([`schema: pg_hardstorage.v1`](../../reference/api/index.md)).
Operators who need a
different shape put a transformer in front — a tiny HTTP
service that re-shapes and forwards. Same-shape-everywhere
keeps the data plane boring on purpose.

Example body:

```json
{
  "schema": "pg_hardstorage.v1",
  "ts": "2026-04-28T14:21:08Z",
  "severity": "warning",
  "component": "wal.stream",
  "op": "wal.lag",
  "deployment": "db1",
  "msg": "WAL replay lag exceeds 30s",
  "details": {"lag_seconds": 47}
}
```

## Auth secret hygiene

The `auth_header` value is loaded into memory at agent start and
never written to logs. Logs that mention the sink redact the
value. **Don't put long-lived bearer tokens in
`pg_hardstorage.yaml`** if you can help it; future versions
support a `kms-secret://` indirection so the actual secret lives
in the keyring (v0.5+ surface).

## Air-gap interaction

The URL host is checked against `airgap.allowlist` at sink-add
time. Under strict mode, only RFC1918 / loopback / explicitly
allowlisted hosts are accepted.

## Reliability

Each emit is a single POST with the configured timeout. There's
**no in-binary retry queue**: a slow or failing endpoint logs a
diagnostic and moves on. If durability matters, point the
webhook at a queue (NATS / Kafka / Redis Streams) and consume
from there.

## Troubleshooting

**`webhook: config.url is required`** — the `--set` was empty.

**`webhook: method must be POST or PUT`** — typo in the method
field.

**`Auth header rejected`** — the bearer token expired. Rotate
out-of-band and `pg_hardstorage notify add webhook` again
(re-adding by name replaces).

**Body shape isn't what you expected** — pg_hardstorage emits
the v1 event schema; your endpoint should parse that, or you
should put a transformer in front. Don't ask the sink to
re-shape — every breaking change there breaks the
24-month JSON contract.

## Next steps

- [Add a Slack sink](sink-slack.md) — most common companion
- [Add a syslog sink](sink-syslog.md) — for SIEM forwarding
- [Stable JSON schema reference](../../reference/api/index.md)
- [`notify` CLI reference](../../reference/cli/pg_hardstorage_notify.md)
