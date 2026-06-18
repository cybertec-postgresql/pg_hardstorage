---
title: Enable air-gap policy
description: Turn on the strict air-gap gate and configure the
              endpoint allowlist.
tags:
  - airgap
  - security
  - policy
---

# Enable air-gap policy

> Flip `pg_hardstorage` into a posture where every outbound
> endpoint is refused unless it resolves to loopback,
> RFC1918/RFC4193 private space, the Tailscale CGNAT range,
> a `file://` / `unix://` scheme, or a host you've added to
> the explicit allowlist. One config line; no code paths
> bypass the gate.

## What you need

- An understanding of which outbound endpoints your
  deployment actually uses (LLM provider, OTLP collector,
  Slack/Jira/PagerDuty sinks, control plane, storage
  backends if HTTP-based).
- The list of in-perimeter FQDNs that resolve outside
  RFC1918 (split-horizon DNS, private VPC endpoints with
  routable hostnames).

## Steps

### 1. Pick a resolution path

The policy comes from one of three sources, in this
precedence order:

```text
flag (--airgapped) > env (PG_HARDSTORAGE_AIRGAPPED=1) >
config (airgapped: strict at top level) > default (off)
```

For one-off invocations, the flag is right. For a host's
permanent posture, the config file is right. The env var
covers `/etc/environment` deployments where every command
should default to strict.

### 2. Enable in config (the steady-state path)

Edit `/etc/pg_hardstorage/pg_hardstorage.yaml`:

```yaml
airgapped: strict

airgap:
  allowlist:
    - llm.internal.example.com
    - otel.internal.example.com:4317
    - sink-jira.internal.example.com
```

Allowlist entries match host or `host:port`. Comparison is
case-insensitive on the host portion; ports (when present
in an entry) must match exactly.

### 3. Verify the gate is on

```bash
pg_hardstorage doctor
```

`doctor` reports the resolved policy in its system section.
The audit log emits a `config.airgap.applied` event with
the resolved mode + allowlist size on every startup.

### 4. (Optional) One-off flag override

```bash
pg_hardstorage --airgapped backup db1
```

Forces strict for this invocation regardless of config.
`--airgapped=false` flips the gate off for one invocation
where config has it on.

## What just happened

`pg_hardstorage` resolves the policy once at startup
(`PersistentPreRunE`) and stores it in a process-wide
`atomic.Value`. Every code path that opens an outbound URL
calls `airgap.Default().EndpointAllowed(rawURL)` and
surfaces a typed `ErrEndpointNotAllowed` on refusal. There's
one arbiter, set once, read everywhere — no library can
silently route around it.

## What the gate allows in strict mode

| Class | Examples | Verdict |
| --- | --- | --- |
| Local schemes | `file://…`, `unix://…`, `fd://…`, `stdio://…` | Always allowed (inherently local) |
| Loopback | `127.0.0.1`, `::1`, `localhost` | Allowed |
| RFC1918 | `10/8`, `172.16/12`, `192.168/16` | Allowed |
| RFC4193 (IPv6 ULA) | `fc00::/7` | Allowed |
| Link-local | `169.254/16`, `fe80::/10` | Allowed |
| CGNAT (Tailscale) | `100.64.0.0/10` | Allowed |
| Allowlist hit | host or `host:port` exact match | Allowed |
| Anything else | publicly-routable IPs, unknown hostnames | Refused |

Hostnames that aren't literal IPs are checked against the
allowlist only — DNS is **deliberately not consulted**:
- making startup wait on DNS would slow the binary,
- DNS poisoning could bypass the gate,
- dev / production split-horizon would give different
  verdicts for the same config.

If your in-perimeter endpoint resolves to a public IP via
split-horizon DNS, add the **hostname** to the allowlist.

## Troubleshooting

### `airgap: hostname "X" is not in the airgap allowlist`

The endpoint URL went through the gate and lost. Either:

- Add the hostname to `airgap.allowlist`, then restart the
  agent / re-run the CLI.
- Switch the endpoint to a loopback / RFC1918 address.
- Confirm the URL string is what you expect (typos in
  config show up here).

The error wraps `ErrEndpointNotAllowed`; programmatic callers
distinguish it via `errors.Is(err, airgap.ErrEndpointNotAllowed)`.

### `airgap: scheme "X" is not recognised`

The gate refuses unknown schemes by default — better to
surface than silently allow. Recognised network schemes:
`http`, `https`, `grpc`, `grpc+tls`, `tcp`, `tcp+tls`, `tls`,
`syslog`, `syslog+tls`. Local-only schemes: `file`, `unix`,
`fd`, `stdio`. Anything else means either a typo or a
genuinely-new transport that we should add.

### Need a third-party LLM in air-gap mode

Use a local provider — `ollama` or a self-hosted vLLM
endpoint — and set the LLM `endpoint:` to a loopback /
RFC1918 / allowlisted host. The provider stack speaks the
OpenAI Chat Completions wire format; pointing at a local
implementation is a config change, not code.

## Verifying the binary doesn't phone home

The binary has zero default outbound calls — no telemetry,
no auto-update checks, no Rekor lookups. The gate exists to
police *operator-configured* outbound endpoints, not because
the binary itself reaches out.

To convince an auditor:

```bash
strace -f -e trace=connect pg_hardstorage doctor 2>&1 \
    | grep -v 'AF_UNIX\|AF_LOCAL'
```

Should be empty, modulo whatever endpoints your config
declares (LLM, sinks, OTLP).

## Next steps

- [Export a repo bundle](repo-bundle-export.md) — the
  air-gap transport for backups + WAL.
- [Import a repo bundle](transport-bundle-import.md) — the
  destination side.
- [Air-gap mode (config reference)](../../reference/cli/pg_hardstorage.md)
  — the CLI flag.
- [Verifier sandbox: Firecracker](../verify/firecracker-sandbox.md)
  — verify backups without any outbound calls.
