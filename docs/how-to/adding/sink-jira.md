---
title: Add a Jira sink
description: Wire pg_hardstorage events to Jira issues with the
              one-ticket-per-recurring-failure dedup posture.
tags:
  - sinks
  - jira
  - ticketing
---

# Add a Jira sink

> The Jira sink creates issues when events fire. The default
> `dedupe_by_subject` strategy reuses an existing open ticket
> for recurring failures and appends a comment, so a flapping
> backup doesn't spawn 47 duplicate tickets.

## What you need

- A Jira instance (Cloud or Server / Data Center).
- A user account with **Create issue** permission on the target
  project. Use a service account, not a person — service
  accounts survive staffing changes.
- An auth credential:
  - **Cloud:** an Atlassian API token created at
    `id.atlassian.com → Security → API tokens`. Pair with the
    account email.
  - **Server / DC:** a Personal Access Token on the user.

## Steps

### 1. Add the sink

```bash
pg_hardstorage notify add jira \
    --name ops-jira \
    --set base_url=https://acme.atlassian.net \
    --set project=OPS \
    --set issue_type=Incident \
    --set email=svc-pg-hardstorage@acme.com \
    --set api_token=$JIRA_TOKEN \
    --min-severity error
```

```console
✓ Sink added — ops-jira (plugin jira)
```

The builder validates `base_url` parses, `project` is set, and
exactly one auth pair (`email + api_token` **or** `bearer_token`)
is present.

### 2. Verify

```bash
pg_hardstorage notify list
```

### 3. Trigger a smoke-test event

Run any failing operation (a verify against a tampered
manifest, for example) and confirm the issue appears in the
project's queue.

## Configuration reference

```yaml
sinks:
  - name: ops-jira
    plugin: jira
    config:
      base_url: https://acme.atlassian.net
      project: OPS
      issue_type: Incident                # default: Incident
      email: svc-pg-hardstorage@acme.com  # cloud basic auth
      api_token: <ATLASSIAN_API_TOKEN>    # cloud basic auth
      # OR for self-hosted / DC:
      # bearer_token: <PAT>
      ticket_strategy: dedupe_by_subject  # dedupe_by_subject | always_new
      labels: ["pg-hardstorage", "automation"]
    filter:
      min_severity: error                 # default: error
```

| Key | Default | Notes |
| --- | --- | --- |
| `base_url` | required | The Jira root, no trailing slash. |
| `project` | required | Project key (e.g. `OPS`). |
| `issue_type` | `Incident` | Must exist in the project's workflow. |
| `email` + `api_token` | — | Cloud basic auth. |
| `bearer_token` | — | Self-hosted / DC PAT. |
| `ticket_strategy` | `dedupe_by_subject` | `always_new` opens a fresh ticket per event. |
| `labels` | `[]` | Applied to every created issue. |

## Ticket strategies

**`dedupe_by_subject`** (default). Recurring failures with the
same identity tuple — `(deployment, op)` — reuse the open ticket
and append a comment. The "exactly one ticket per recurring
failure" posture from the spec.

**`always_new`**. Each event opens a fresh ticket. Useful when
events have independent significance (audit emission, GDPR
DSAR notices) and you want a one-to-one paper trail.

## Severity floor

`min_severity: error` is the right default — Jira is for things
that need a human follow-up, not a chat-room ping. Slack /
PagerDuty floors govern the noisier tiers; let Jira stay
quiet unless something's broken.

## Auth choice

Cloud workspaces should use the email + API-token pair.
Self-hosted / Data Center deployments use a PAT
(`bearer_token`). Both flows hit the same `/rest/api/3/issue`
endpoint; the auth header is the only difference.

## Troubleshooting

**`jira: config.base_url is required`** — `--set base_url=...`
missing.

**`jira: exactly one auth must be supplied`** — supplied both
`api_token` and `bearer_token`, or neither.

**`jira: 401 Unauthorized` on emit** — the token expired or the
account got disabled. Rotate via the Atlassian console.

**`jira: 403 Forbidden`** — the account lacks `Create issues`
on the project. Adjust project permissions.

**`jira: 404` on the project key** — wrong key, or the
service-account user has no browse permission. Both surface as
404 to a non-browsing principal.

**Recurring failures spawn separate tickets** — the
`dedupe_by_subject` lookup is matching against subject text;
if the event's subject changes minute-to-minute (e.g. it
embeds a dynamic backup ID), tickets won't dedupe. Pair with
a stable subject or switch to `always_new`.

## Next steps

- [Add a PagerDuty sink](sink-pagerduty.md) — page on top of
  ticket
- [Add a Slack sink](sink-slack.md) — chat alongside Jira
- [`notify` CLI reference](../../reference/cli/pg_hardstorage_notify.md)
