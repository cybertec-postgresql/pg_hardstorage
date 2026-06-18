# llm/

The TUI chat, MCP server, and skills loader that make `pg_hardstorage`
answerable in natural language without ever giving an LLM the keys to the
cluster.

## What lives here

A constrained chat surface that converses with a provider
(`internal/plugin/llmprovider`), a tool-use layer that maps model calls to
read-only CLI invocations, a safety/privacy filter chain that redacts before any
byte leaves the host, and a YAML-declared skills system. Also an MCP server so
external assistants (Claude Desktop, IDE plugins) can reach the same tool
surface over stdio.

## Key files / subdirs

- `chat/` — REPL session, validator-retry loop, anti-hallucination guards
- `tools/` — tool registry; `cli_runner.go` shells out only to the allowlisted
  `pg_hardstorage` subcommands; `livestate.go` snapshots health
- `safety/` — destructive-action gate, anomaly heuristics, refusal taxonomy
- `privacy/` — strict / standard / open / local-only modes; PII + DSN redactor
- `mcp/` — Model Context Protocol stdio server (third-party clients)
- `skills/` — YAML skill loader;
  `builtin/{ask,explain,incident,restore}.skill.yaml` ship; `runbook` and
  `postmortem` are v1.0 deferred
- `docs/` — embedded runbooks + grounding corpus the model retrieves against
- `history/` — conversation persistence + summarisation (`derive.go`)

## Read next

- `../plugin/llmprovider/README.md` — wire-level provider plumbing
- `../approval/` — every destructive intent routes through here, not the chat
- `../audit/` — every tool call is journaled

## Don't put X here

- Anything that mutates cluster state — wrap it in an `internal/approval`
  workflow first.
- Provider-specific HTTP code — that's `internal/plugin/llmprovider`.
- Free-form prompts that bypass `skills/` — every conversation must be
  anchored to a declared skill.
