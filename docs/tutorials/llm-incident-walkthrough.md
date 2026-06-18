---
title: LLM incident walkthrough
description: Triage a failed backup with the built-in LLM helper and
              ship a signed evidence bundle to the audit log.
tags:
  - llm
  - incident
  - audit
---

# LLM incident walkthrough

> Walks through using `pg_hardstorage llm` to triage a real incident:
> a backup that fails because the replication slot is missing. You
> drop into the `incident` skill, walk through the assistant's
> response, and export a signed evidence bundle of the entire
> session. About 15 minutes.

The LLM helper is a **read-only triage assistant**, not an autopilot.
The default execution mode is `read-only`: the assistant can run
`read_*` and `list_*` tools to inspect cluster state, but
`execute_command` refuses every invocation. Remediation is always the
operator's call. Every prompt, tool call, response, and exit is
audit-logged with a hash chain so you can show your auditor exactly
what was inspected.

---

## What you need

- A working `pg_hardstorage` install (see
  [getting-started](getting-started.md)) and a sandbox repo at
  `file:///tmp/hs-llm-repo`.
- An OpenAI-compatible LLM endpoint *or* a local Ollama at the
  default `:11434`. The default provider is `openai` when an API
  key is in scope; otherwise the CLI refuses with
  `llm.no_provider_configured` and prints the exact env vars to
  set. Pass `--provider mock` for the canned-reply path used by
  this walkthrough's plumbing checks.

For a fully local setup with Ollama:

```bash
ollama pull llama3.1:8b
export PG_HARDSTORAGE_URL=http://127.0.0.1:11434/v1   # Ollama's OpenAI-compat endpoint
export PG_HARDSTORAGE_LLM_KEY=ollama                  # any non-empty value
export PG_HARDSTORAGE_LLM_MODEL=llama3.1:8b
pg_hardstorage llm
```

For OpenAI:

```bash
export PG_HARDSTORAGE_LLM_KEY='sk-proj-...'
# PG_HARDSTORAGE_LLM_MODEL defaults to gpt-4o-mini; override
# if your account uses a different model.
pg_hardstorage llm
```

For the mock provider (no network, deterministic answers, useful for
this walkthrough):

```bash
export PG_HARDSTORAGE_LLM_PROVIDER=mock
```

See [Configure the LLM helper](../how-to/configure-llm.md) and the
annotated reference at `share/pg_hardstorage.sample.yaml` for the
full configuration surface (Anthropic via OpenAI-compat, Azure
OpenAI, OpenRouter, vLLM, ...).

---

## Steps

### 1. Set up a deployment that will fail predictably

The fastest way to make a backup fail is to point it at a healthy
PostgreSQL but block the replication slot. Start a sandbox PG and
take one good backup so the deployment exists:

```bash
docker run -d --name hs-llm-pg -e POSTGRES_PASSWORD=postgres \
    -p 5432:5432 postgres:17

pg_hardstorage repo init file:///tmp/hs-llm-repo

pg_hardstorage init \
    --pg-connection "${PG_CONNECTION:-postgres://postgres:postgres@127.0.0.1/postgres}" \
    --repo file:///tmp/hs-llm-repo \
    --deployment db1 \
    --yes
```

Now drop the slot from underneath the agent (this is the same shape
as a Patroni-failover or a curious DBA running
`SELECT pg_drop_replication_slot(...)`):

```bash
PGPASSWORD=postgres psql -h 127.0.0.1 -U postgres -c \
    "SELECT pg_drop_replication_slot('pg_hardstorage_db1');"
```

### 2. Reproduce the failure

```bash
pg_hardstorage wal stream db1 \
    --pg-connection "${PG_CONNECTION:-postgres://postgres:postgres@127.0.0.1/postgres}" \
    --repo file:///tmp/hs-llm-repo
```

```console
ERROR: WAL stream replication slot 'pg_hardstorage_db1' is not present on the server.
What to do: the slot was probably dropped by an admin. Recreate it
with:
  pg_hardstorage wal repair db1
```

The exit code is non-zero and the structured event ID is
`wal_gap_detected`. You could `pg_hardstorage wal repair` blindly —
or you can ask the helper to talk you through it.

### 3. Drop into the incident skill

```bash
pg_hardstorage llm chat --audit-repo file:///tmp/hs-llm-repo
```

The `--audit-repo` flag captures
every `llm.*` event into the named repo's audit chain so you can
export a signed evidence bundle later.

```console
[AI assistant — verify every suggestion before running]
skill: incident v1.0.0 · provider: mock · model: (provider default)
url: (provider default)
/help for commands · /exit to quit

  (mock provider — replies are stub echoes; pass --provider openai for a real model)

> 
```

The `model:` and `url:` lines show what the chat is
actually talking to.  When colour output is supported
(real TTY, `NO_COLOR` not set, `TERM` not `dumb`) the
field labels render in cyan and values in bold; piping
the banner to a file or `less` produces plain text.

### 4. Tell it what happened

```text
> The wal stream for db1 just failed with "replication slot is not
> present on the server". Walk me through what to do.
```

A response from a real provider will:

1. Call `read_doctor` to check the deployment's health snapshot.
2. Call `read_status db1` to confirm last-known WAL LSN.
3. Match the `wal_gap_detected` event class against the runbook
   index and surface [R6 — Slot dropped, gap detected](../reference/runbooks/R6-slot-dropped-gap.md).
4. Suggest `pg_hardstorage wal repair db1` and explain the
   side-effects (the repaired slot bootstraps from the latest
   committed segment; any LSN gap is recorded as a `wal_gap` audit
   event).

The helper does **not** run `wal repair`. It surfaces it and stops.

### 5. Inspect transparency under your fingers

The slash commands print exactly what the assistant has access to:

```text
> /show-skill
incident · v1.0.0 · /home/you/.config/pg_hardstorage/skills/incident.skill.yaml
```

```text
> /show-tools
read_doctor       (read-only, preloaded)
read_status       (read-only, preloaded)
list_deployments  (read-only, preloaded)
list_backups
read_backup
read_repo_usage
read_audit
list_runbooks
read_runbook
search_docs
suggest_command
preview_command
```

```text
> /show-budget
tokens used: 4128 · budget: 120000 · 116000 remaining
```

`execute_command` is not in the list — the skill's
`permissions.read_only: true` means the runner refuses every mutation
even if the model asks. This is the design.

### 6. Apply the fix yourself

Leave the chat open with `Ctrl-Z` (or `/exit` to close) and run the
suggestion in another terminal:

```bash
pg_hardstorage wal repair db1 \
    --pg-connection "${PG_CONNECTION:-postgres://postgres:postgres@127.0.0.1/postgres}" \
    --repo file:///tmp/hs-llm-repo
```

Confirm the slot is back:

```bash
pg_hardstorage doctor db1
```

```console
db1 — PG 17.x — primary @ 127.0.0.1
  ✓ PostgreSQL reachable
  ✓ Replication slot 'pg_hardstorage_db1' active
  ✗ WAL gap recorded: [0/1A000000 .. 0/1B000000]
    Suggested fix: pg_hardstorage wal repair db1 --repo <url> --pg-connection <conn>
```

The recorded gap is the exact range during which the slot was
absent. You can chase it with `wal audit` or accept it as a known
recovery limitation depending on your RPO target.

### 7. Export the signed evidence bundle

Note the session ID printed in the chat header (`session=01HXX...`)
and pass it to `llm export-session`:

```bash
pg_hardstorage llm export-session 01HXX... \
    --repo file:///tmp/hs-llm-repo \
    --out /tmp/hs-llm-incident.tar.gz
```

```console
✓ exported 14 audit events
✓ Merkle root anchored: sha256:c1aa...
✓ wrote /tmp/hs-llm-incident.tar.gz (signed, ed25519)
```

Inside the tarball:

```text
session.json              # session metadata (skill, provider, principal)
events.ndjson             # every llm.* event in order, hash-chained
manifest.json             # bundle index + audit chain head
manifest.sig              # ed25519 signature over manifest.json
```

This is the artefact you hand to the auditor: **what the operator
was told, by which model, with which tools available, and what the
operator chose to do with that advice.**

### 8. Tear down

```bash
docker rm -f hs-llm-pg
rm -rf /tmp/hs-llm-repo /tmp/hs-llm-incident.tar.gz
```

---

## What just happened

You used the LLM helper as a triage cone — narrowing
"WAL stream failed" to a specific runbook with the cluster's actual
state in scope, without granting the model any mutation power. Every
turn was captured into the audit chain, and the export step bundled
it into a portable signed artefact.

Three things to internalise:

- **`read-only` is the default for a reason.** `advise+execute` exists
  for tightly-scoped automation, but every gate (n-of-m approval,
  insider-threat scan, audit chain) is still in the path.
- **Skills are configurable.** Drop a YAML under
  `<config>/skills/<name>.skill.yaml` and `pg_hardstorage llm skill install`
  it; the precedence chain (operator overlay > tenant > builtin)
  applies.
- **`auto_on_error`** can launch the matching skill the instant a
  structured error fires. Set `--on-error-llm` once or
  `PG_HARDSTORAGE_ON_ERROR_LLM=1` in the agent's env, and the next
  `wal_gap_detected` opens an incident chat directly.

---

## Next steps

- [R6 — Slot dropped, gap detected](../reference/runbooks/R6-slot-dropped-gap.md) —
  the runbook the helper surfaced.
- [Operator guide — Troubleshooting](../operations/troubleshooting.md) —
  the quick-reference index of error → fix.
- [Architecture tour](../explanation/architecture-tour.md) — where the
  LLM helper sits in the data plane.
