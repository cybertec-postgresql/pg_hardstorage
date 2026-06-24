---
title: LLM provider plugin contract
description: The Provider interface — chat completion backends with optional streaming and tool support.
tags:
  - plugins
  - llm
  - reference
---

# LLM provider plugin contract

An LLM provider answers a chat completion request.  The
interface is deliberately small — `Chat` takes a list of
messages and returns a stream of chunks.  Tool-calling is
layered on top via `SupportsTools()`; the orchestrator in
`internal/llm/chat` handles prompt construction, message
history, and tool dispatch.

v1.0 ships exactly two providers in-tree:

- **`mock`** — for tests; canned responses.
- **`openai`** — speaks the OpenAI Chat Completions wire
  format and routes every backend through that one shape:
  `api.openai.com` (default), Azure OpenAI (with
  `api_key_header=api-key`), Ollama (`http://127.0.0.1:11434/v1`),
  vLLM, OpenRouter, Together, Groq, LM Studio,
  llama.cpp server.

Older Anthropic-native and Ollama-native providers were
removed before v1.0 (audit-driven simplification): one wire
format, one set of tests, one deployment story including
the air-gapped path.

!!! note "Reference implementations"
    - `internal/plugin/llmprovider/openai.go` — the
      production provider (every operator-visible
      backend).
    - `internal/plugin/llmprovider/mock.go` — for tests
      and as a minimal example of the interface.
    Both are short and self-contained.

## Interface

```go
// internal/plugin/llmprovider/llmprovider.go

package llmprovider

type Provider interface {
    Name() string
    Open(ctx context.Context, cfg ProviderConfig) error
    Chat(ctx context.Context, msgs []Message, tools []ToolDef) iter.Seq2[Chunk, error]
    SupportsTools() bool
    SupportsStreaming() bool
    Close() error
}
```

## Per-method contract

### `Name() string`

Stable identifier — `"mock"`, `"openai"`.  Used for
telemetry labels (`pg_hardstorage_llm_request_total{provider="openai"}`).
Stable across versions; goes into audit-log
`subject.llm_provider`.

### `Open(ctx context.Context, cfg ProviderConfig) error`

Initialise.  Idempotent on repeat calls; a config change
requires a fresh `Provider` instance (re-construct via the
registry's builder).

```go
type ProviderConfig struct {
    Endpoint string         // base URL or DSN
    Model    string         // provider-specific model id
    APIKey   string         // already-resolved (kms-secret expansion is upstream)
    Extra    map[string]any // provider-specific overrides
}
```

`APIKey` is **already resolved** — `kms-secret://`
indirection happens in the chat orchestrator's config
resolver, before this method is called.  Providers
should treat `APIKey` as opaque and never log it.

`Extra` is the operator's `pg_hardstorage.yaml`
`llm.config.extra:` block — a free-form map for
provider-specific options the canonical fields don't
cover (Azure deployment names, Ollama keep-alive, OpenAI
organization IDs).

### `Chat(ctx, msgs, tools) iter.Seq2[Chunk, error]`

Issue a chat completion.  Returns a Go-1.23 range-over-
function iterator that yields `Chunk` values until
`Done == true` (success) or an error (terminal failure).

The orchestrator iterates:

```go
for chunk, err := range provider.Chat(ctx, msgs, tools) {
    if err != nil { return err }
    // ... stream to renderer, accumulate text, dispatch tools ...
    if chunk.Done {
        // record Usage if present
        break
    }
}
```

`ctx` cancellation MUST abort the underlying HTTP request
promptly.  A long-tail `Chat` blocking the orchestrator
on shutdown is the most common provider bug; use
`http.NewRequestWithContext` and propagate the context
through every layer.

### Message shape

```go
type Message struct {
    Role    string `json:"role"`              // "system", "user", "assistant"
    Content string `json:"content,omitempty"` // text content (mutually exclusive with ToolCall / ToolResult)

    // assistant turn that invoked a tool:
    ToolCall *ToolCallChunk `json:"tool_call,omitempty"`

    // user turn carrying a tool's return value back:
    ToolUseID  string `json:"tool_use_id,omitempty"`   // pairs with assistant's ToolCall.ID
    ToolResult string `json:"tool_result,omitempty"`   // tool's output (typically JSON)
    Name       string `json:"name,omitempty"`          // tool name, for backends that key on it
}
```

A message has either `Content` OR a `ToolCall` OR a
`ToolResult`.  The chat orchestrator persists all three
shapes in conversation history so a provider re-rendering
sees the full round-trip.

### Tool definitions

```go
type ToolDef struct {
    Name        string         `json:"name"`
    Description string         `json:"description"`
    Schema      map[string]any `json:"schema,omitempty"` // JSON Schema for arguments
}
```

The orchestrator builds these from the active skill's
`available_tools` list; providers that report
`SupportsTools() == false` see `tools` as an empty slice
they can ignore.

### Chunk shape

```go
type Chunk struct {
    Text     string         `json:"text,omitempty"`      // delta text
    ToolCall *ToolCallChunk `json:"tool_call,omitempty"` // model wants to invoke a tool
    Done     bool           `json:"done,omitempty"`      // terminal chunk
    Usage    *Usage         `json:"usage,omitempty"`     // token counts on final chunk
}

type ToolCallChunk struct {
    ID   string         `json:"id,omitempty"`   // provider-issued correlation token
    Name string         `json:"name"`
    Args map[string]any `json:"args"`
}

type Usage struct {
    PromptTokens     int `json:"prompt_tokens"`
    CompletionTokens int `json:"completion_tokens"`
    TotalTokens      int `json:"total_tokens"`
}
```

`ToolCallChunk.ID` is the provider's correlation token
(Anthropic-native: `tool_use_id`; OpenAI: the function
call ID).  The orchestrator includes the same ID on the
matching `ToolResult` message so providers that need to
pair them up can.  Empty string for providers that don't
issue IDs.

`Usage` MAY appear only on the final chunk (`Done = true`)
or interleaved.  The orchestrator forwards it to
`pg_hardstorage_llm_tokens_total`.

### `SupportsTools() bool`

Reports whether the provider can invoke `ToolDef`s.  The
orchestrator gates skill-specific tool routing on this
(skills that require tool-calling refuse to start
against a `SupportsTools()==false` provider).

### `SupportsStreaming() bool`

Reports whether `Chat` actually streams chunks vs.
buffering and emitting one final chunk.  The mock and
openai providers stream; some Bedrock InvokeAPI shapes
don't.  Used by the orchestrator's UX layer to decide
whether to render incremental text.

### `Close() error`

Release HTTP connections, idle clients, refresh tokens.
Idempotent.

## Streaming contract

For `SupportsStreaming() == true` providers:

- Each `Chunk` represents an INCREMENTAL delta, not the
  cumulative response.  Concatenate `chunk.Text` across
  iterations to assemble the full response.
- `Done = true` arrives exactly once, on the final chunk.
- A chunk MAY carry `Text` AND `ToolCall` AND `Usage`
  AND `Done` simultaneously; the orchestrator handles
  each field independently.

For non-streaming providers:

- A single chunk with the full `Text`, `Done = true`,
  and (if available) `Usage`.

## Tool-call dispatch flow

```
1. Orchestrator builds messages + tools, calls Chat.
2. Provider streams chunks; eventually emits a chunk with ToolCall set.
3. Orchestrator receives the ToolCall, executes the named tool,
   captures the result string.
4. Orchestrator appends two messages to history:
     a. assistant message with .ToolCall set (the call)
     b. user message with .ToolUseID + .ToolResult set (the result)
5. Orchestrator re-invokes Chat with the new history.
6. Provider sees both messages and continues the conversation.
```

The provider's job is **shape-translation**: incoming
canonical `Message` slice → wire format on the way out;
wire response → `Chunk` stream on the way back.  The
orchestrator owns history, retry logic, tool dispatch,
and skill orchestration.

## Registration

```go
func init() {
    llmprovider.DefaultRegistry.Register("openai", func() llmprovider.Provider {
        return New()  // returns a fresh, unopened Provider
    })
}
```

The `Builder` returns a *fresh* provider; the orchestrator
calls `Open(ctx, cfg)` after retrieval.  Re-registration
overwrites — the idiom for operator-supplied overrides
via Tier-2 plugins.

`Register` panics on a nil builder or empty name; both
are programmer errors.

## Error sentinels

```go
var ErrUnknownProvider = errors.New("llmprovider: unknown provider")
```

Returned by `Registry.Get(name)` when no builder is
registered.  The orchestrator's startup wiring catches
this and surfaces a useful "registered providers: …"
message.

## Concurrency contract

A `Provider` instance MAY be shared across goroutines for
concurrent `Chat` calls — the orchestrator currently
serializes calls per session, but that's not contractual.
HTTP-based providers naturally handle this via shared
`*http.Client`.

`Open` and `Close` are serial; the host serializes
against in-flight `Chat`.

## Air-gap interaction

The OpenAI-shaped Endpoint MAY point at a local model
runtime (Ollama, vLLM, LM Studio, llama.cpp).  Operators
in air-gap deployments (`PG_HARDSTORAGE_AIRGAPPED=1`)
point at a private-IP endpoint; the
`airgap.Default().EndpointAllowed(url)` check happens in
the chat orchestrator's config resolver, before
`Provider.Open` is called.

Provider implementations don't need to consult the
air-gap policy directly — the orchestrator gates the
endpoint up-front.

## What providers MUST get right

1. **Context cancellation aborts the HTTP round-trip.**
   No `Chat` blocks past `ctx.Done()`.
2. **Usage reported when available.**  The
   `pg_hardstorage_llm_tokens_total` metric depends on
   this; absent Usage means absent telemetry.
3. **Tool-call IDs round-trip.**  If the provider issues
   IDs, propagate them; the orchestrator correlates
   them.
4. **`APIKey` never logs.**  Treat as opaque.

## Tier-2 mapping

The Tier-2 gRPC contract for LLM providers
(`PluginTier.PLUGIN_TIER_LLM_PROVIDER`) is forward-
looking; no proto service is defined for it in
`proto/plugin/v1/plugin.proto` v1.  v1.1 will add one.
Tier-1 is the only path today.

## Further reading

- Skill schema: `reference/skill-schema.md`.
- Chat orchestrator: `internal/llm/chat/`.
- LLM telemetry catalogue: `reference/metric-catalogue.md`
  (filter by `llm`).
