---
title: Add a syslog sink
description: Emit RFC 5424 syslog over UDP, TCP, or TLS for SIEM
              ingestion.
tags:
  - sinks
  - syslog
  - siem
---

# Add a syslog sink

> The syslog sink emits RFC 5424 messages over UDP, TCP, or
> TLS. RFC 5424 is the modern format every SIEM expects;
> stream transports use octet-counted RFC 6587 framing so
> binary-safe readers (`rsyslog`, `syslog-ng`, Splunk UF,
> Vector) parse without re-tokenising.

## What you need

- A syslog destination — `rsyslog`, `syslog-ng`, a Splunk HEC
  via UF, a Vector aggregator, or a SIEM listener.
- The host:port pair and the chosen transport (`udp`, `tcp`,
  `tls`).
- For `tls`: a CA bundle the listener's certificate chains to.

## Steps

### 1. Add the sink — TLS to a SIEM

```bash
pg_hardstorage notify add syslog \
    --name prod-syslog \
    --set protocol=tls \
    --set address=siem.example.com:6514 \
    --set facility=local6 \
    --min-severity notice
```

```console
✓ Sink added — prod-syslog (plugin syslog)
```

`--set` covers top-level keys only.  Add the nested `tls:`
block (CA bundle, mTLS keypair, server-name override) by
editing `pg_hardstorage.yaml` directly — see the
configuration reference at the bottom of this page for the
shape.  Validate with `pg_hardstorage doctor` after every
edit.

### 2. UDP to a local relay (testing)

```bash
pg_hardstorage notify add syslog \
    --name local-syslog \
    --set protocol=udp \
    --set address=127.0.0.1:514 \
    --set facility=local6
```

### 3. Verify

```bash
pg_hardstorage notify list
```

### 4. Smoke-test

```bash
pg_hardstorage verify db1 latest --repo file:///srv/pg_hardstorage/repo
```

```console
# example tcpdump on the listener
<182>1 2026-04-28T14:21:08.000Z host01 pg_hardstorage 12345 verify [meta=…] {"schema":"pg_hardstorage.v1",...}
```

The angle-bracketed PRI is `<facility*8 + severity>` (182 =
local6 + notice).

## Configuration reference

```yaml
sinks:
  - name: prod-syslog
    plugin: syslog
    config:
      protocol: tls                      # udp | tcp | tls
      address: siem.example.com:6514     # host:port
      facility: local6                   # default
      app_name: pg_hardstorage           # APP-NAME field; default
      hostname: ""                       # default: os.Hostname()
      timeout: 5s                        # connect + write timeout
      tls:                               # required when protocol=tls
        ca_file: /etc/ssl/siem-ca.pem    # PEM bundle of trusted CAs (optional; falls back to system roots)
        cert_file: /etc/ssl/client.pem   # client cert for mTLS (optional, must come with key_file)
        key_file: /etc/ssl/client.key    # client key for mTLS
        server_name: siem.acme.internal  # SNI / cert-name override (optional)
        min_version: tls1.2              # tls1.2 | tls1.3 (default tls1.2)
        insecure_skip_verify: false      # opt-out of cert verification (TEST ONLY)
    filter:
      min_severity: notice               # default
```

| Key | Default | Notes |
| --- | --- | --- |
| `protocol` | `udp` | `udp`, `tcp`, or `tls`. UDP is fire-and-forget; TCP and TLS reconnect on transient failure. |
| `address` | required | `host:port`. |
| `facility` | `local6` | `kern`, `user`, `local0`-`local7`. |
| `app_name` | `pg_hardstorage` | Maps to the APP-NAME field. |
| `hostname` | `os.Hostname()` | Override only for canonical-name aliasing. |
| `timeout` | `5s` | Connect + write timeout per attempt. |
| `min_severity` | `notice` | RFC 5424 floor. |
| `tls.ca_file` | system roots | PEM bundle of trusted CAs (private CA / corp PKI). |
| `tls.cert_file` + `tls.key_file` | none | Client cert + key for mTLS. Must be set together. |
| `tls.server_name` | host portion of `address` | SNI + cert-name match override. |
| `tls.min_version` | `tls1.2` | `tls1.2` or `tls1.3`. |
| `tls.insecure_skip_verify` | `false` | Disable cert verification. **Test deployments only.** |

The whole `tls:` block is rejected when `protocol` is not
`tls` — fails loudly so audit events never silently downgrade
to plaintext.

## Why RFC 5424 (not 3164)

RFC 5424 preserves PRI / hostname / app / proc / msgid as
**structured fields**, so downstream pipelines don't have to
re-parse our JSON message body. RFC 3164 — the BSD-era
"syslog-classic" format — drops most of that and routinely
truncates messages at 1024 bytes. Modern SIEMs reject it on
sight.

The MSG-PART is the JSON-encoded Event (schema
`pg_hardstorage.v1`). Want CEF or LEEF instead? Configure a
[`cef` or `splunkhec` sink](sink-other.md) — the syslog sink
itself stays neutral.

## Stream framing

For `tcp` and `tls`, messages are wrapped with octet-counted
framing per RFC 6587:

```text
<MSGLEN> <SP> <SYSLOG-MSG>
```

`MSGLEN` is the byte count of `SYSLOG-MSG`. Binary-safe
readers handle this without re-tokenising on `\n`. UDP has no
framing — one datagram per message.

## Severity floor

`min_severity: notice` is the right default for a SIEM feed —
captures every operational event but discards the
`info` / `debug` firehose. For audit-only pipelines, set
`min_severity: error`. For full forensic capture, set
`debug`.

## Troubleshooting

**`syslog: unknown facility` at builder time** — typo in the
facility name. Allowed: `kern`, `user`, `local0`-`local7`.

**TCP connection refused** — listener not bound, or firewall
blocking. The sink reconnects with backoff; check the agent's
journal.

**TLS certificate verification failed** — supply the listener's
CA bundle via `tls.ca_file`. The system root store is consulted
by default. For SNI / cert-name mismatches (the listener
presents a name different from the address host), set
`tls.server_name`. As a last resort for test rigs only,
`tls.insecure_skip_verify: true` bypasses verification — never
in production.

**SIEM dropping messages** — check the SIEM's max-message-size
setting; pg_hardstorage's JSON bodies can exceed 4 KiB on
verbose components. Most modern SIEMs accept up to 64 KiB on
TLS / TCP transports.

## Next steps

- [Add a webhook sink](sink-webhook.md) — for non-syslog HTTP
- [Other built-in sinks](sink-other.md) — CEF, OpenTelemetry,
  Splunk HEC, Datadog, etc.
- [`notify` CLI reference](../../reference/cli/pg_hardstorage_notify.md)
