# plugin/llmprovider/

The LLM-provider tier: chat backends the `internal/llm` package talks to. Two
implementations cover every model `pg_hardstorage` can reach today.

## What lives here

The `Provider` interface — a small, streaming, tool-use-aware contract — and
its shipping implementations. The `openai` provider doubles as a generic
OpenAI-compatible client, which makes Ollama, vLLM, and llama-cpp's OpenAI
endpoint all reachable through it without a new plugin.

## Provider interface

`Chat(ctx, []Message, []ToolDef) iter.Seq2[ChatChunk, error]` — Go 1.23
range-over-func streaming. `Name()`, `Models() []string`, `Close()`. Tool calls
are surfaced as typed chunks; the caller (`internal/llm/chat`) drives the
tool-use loop.

## Plugins

| Name | Scope | Status |
| --- | --- | --- |
| `anthropic` | Claude family via the Anthropic Messages API | real |
| `openai` | OpenAI + any OpenAI-compatible (Ollama, vLLM, llama-cpp) | real |

## Key files

- `llmprovider.go` — `Provider`, `Message`, `ToolDef`, `ChatChunk`, registry
- `openai.go` / `openai_test.go` — OpenAI-compatible client; configurable base
  URL
- `mock.go` / `mock_test.go` — deterministic mock for `internal/llm` unit
  tests
- `anthropic` lives in its own file alongside (see siblings)

## Read next

- `../../llm/README.md` — the consumer
- `../../llm/safety/` — the gate every tool call runs through
- `docs/how-to/configure-llm.md` — provider-by-provider setup, including local
  Ollama

## Don't put X here

- Prompt construction or skill loading — that's `internal/llm/skills` +
  `chat`.
- Tool implementations — `internal/llm/tools`; this tier only declares the
  schema.
- Persistence (history, transcripts) — `internal/llm/history`.
