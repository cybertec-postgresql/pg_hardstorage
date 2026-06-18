// Package llmprovider declares the LLMProvider plugin tier.
//
// A provider answers a chat completion request. The interface is
// deliberately small — Chat takes a list of messages and returns a
// stream of chunks. Tool-calling is layered on top (the provider
// declares whether it supports it via SupportsTools); the orchestrator
// in internal/llm/chat handles the prompt construction, message
// history, and tool dispatch.
//
// + ships exactly two providers:
//
//   - Mock (in-tree) — for tests; returns canned responses.
//
//   - OpenAI (in-tree) — speaks the OpenAI Chat Completions wire
//     format and routes every backend through that one shape.
//     api.openai.com is the default; operators point at any
//     OpenAI-compatible service via Endpoint:
//
//   - Azure OpenAI     (with api_key_header=api-key)
//
//   - Ollama           (http://127.0.0.1:11434/v1)
//
//   - vLLM / lmdeploy  (wherever they're hosted)
//
//   - OpenRouter       (https://openrouter.ai/api/v1)
//
//   - Together / Groq  (their OpenAI-compatible /v1 endpoints)
//
//   - LM Studio / llama.cpp server
//
// Older Anthropic-native and Ollama-native providers were
// removed (audit-driven simplification): one wire
// format, one set of tests, one deployment story including the
// air-gapped path.
package llmprovider

import (
	"context"
	"errors"
	"iter"
)

// Message is one turn in a chat conversation. Role is one of the
// canonical conversation roles ("system", "user", "assistant"); the
// orchestrator translates these to whichever shape the provider
// expects on the wire.
//
// A message has either Content (text) OR a ToolCall (the assistant
// turn where the model invoked a tool) OR a ToolResult (the user
// turn carrying a tool's return value back to the model).  The
// chat orchestrator persists all three shapes in the conversation
// history so a provider re-rendering the history sees the full
// round-trip.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content,omitempty"`

	// Name is set on tool-result messages so a future tool-calling
	// provider can correlate the response with the call.  Older
	// providers (Ollama in v0.1) ignore it.
	Name string `json:"name,omitempty"`

	// ToolCall is set on assistant messages where the model
	// requested a tool invocation rather than emitting text.
	// The chat orchestrator persists these so the provider can
	// re-render the assistant turn correctly on subsequent
	// requests.
	ToolCall *ToolCallChunk `json:"tool_call,omitempty"`

	// ToolUseID + ToolResult mark a "user" message that's
	// actually carrying a tool's return value back to the model.
	// ToolUseID pairs with the assistant turn's ToolCall.ID.
	// ToolResult is the tool's stringified output (typically
	// JSON).
	ToolUseID  string `json:"tool_use_id,omitempty"`
	ToolResult string `json:"tool_result,omitempty"`
}

// ToolDef declares a tool the provider may invoke. The orchestrator
// builds these from the active skill's available_tools list. v0.1
// providers don't actually invoke tools (read-only mode) but the
// shape ships now so the surface stays stable.
type ToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Schema      map[string]any `json:"schema,omitempty"`
}

// Chunk is one streaming response delta. Text accumulates the
// completion; ToolCall is set when the provider wants the caller to
// run a tool.
type Chunk struct {
	Text     string         `json:"text,omitempty"`
	ToolCall *ToolCallChunk `json:"tool_call,omitempty"`
	Done     bool           `json:"done,omitempty"`

	// Usage is set on the final chunk when the provider reports
	// token counts. The orchestrator forwards these to the
	// pg_hardstorage_llm_tokens_total metric.
	Usage *Usage `json:"usage,omitempty"`
}

// ToolCallChunk is a request from the provider to invoke a tool.
// ID is the provider-issued correlation token (Anthropic's
// `tool_use_id`); the orchestrator includes it on the matching
// ToolResult message so the provider can pair them up.  Empty for
// providers that don't issue IDs.
type ToolCallChunk struct {
	ID   string         `json:"id,omitempty"`
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

// Usage is the token-accounting summary.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ProviderConfig is the per-provider configuration block. The
// pg_hardstorage.yaml entry under `llm.config` becomes this map; each
// provider casts the fields it cares about.
type ProviderConfig struct {
	Endpoint string         // base URL or DSN
	Model    string         // provider-specific model id
	APIKey   string         // already-resolved (kms-secret expansion happens upstream)
	Extra    map[string]any // provider-specific overrides
}

// Provider is the LLM-backend plugin contract.
type Provider interface {
	// Name returns the provider's stable identifier ("mock",
	// "openai", ...).  Used for telemetry labels. + ships
	// "mock" + "openai"; the latter handles every backend
	// (OpenAI, Azure, Ollama via /v1, vLLM, OpenRouter, …) by
	// pointing Endpoint at the right base URL.
	Name() string

	// Open initialises the provider with cfg. Idempotent on repeat
	// calls; a config change requires a new Provider instance.
	Open(ctx context.Context, cfg ProviderConfig) error

	// Chat issues a chat completion. The returned iterator yields
	// chunks until Done=true or an error. Cancellation via ctx
	// must abort the underlying HTTP request promptly.
	Chat(ctx context.Context, msgs []Message, tools []ToolDef) iter.Seq2[Chunk, error]

	// SupportsTools reports whether the provider can invoke ToolDefs.
	// v0.1 returns false everywhere;+ providers that implement
	// tool-calling flip it.
	SupportsTools() bool

	// SupportsStreaming reports whether the provider streams chunks
	// vs returning one final chunk. mock streams; ollama streams;
	// some providers (bedrock invoke API) don't.
	SupportsStreaming() bool

	// Close releases provider resources (HTTP connections, etc.).
	Close() error
}

// Builder constructs a Provider. Each provider self-registers via
// init() against DefaultRegistry.
type Builder func() Provider

// Registry maps a provider name to its constructor. Providers register
// at init time; the orchestrator looks up by name when reading config.
type Registry struct {
	builders map[string]Builder
}

// NewRegistry returns an empty registry. Most callers use
// DefaultRegistry; tests construct their own when they need
// isolation.
func NewRegistry() *Registry {
	return &Registry{builders: map[string]Builder{}}
}

// Register adds a builder. Re-registering the same name overwrites —
// intentional for operator-supplied overrides via Tier-2 plugins.
func (r *Registry) Register(name string, b Builder) {
	if name == "" || b == nil {
		panic("llmprovider: Register requires a non-empty name and non-nil builder")
	}
	r.builders[name] = b
}

// Get returns a fresh Provider for the named registration. Returns
// ErrUnknownProvider when no builder is registered.
func (r *Registry) Get(name string) (Provider, error) {
	b, ok := r.builders[name]
	if !ok {
		return nil, ErrUnknownProvider
	}
	return b(), nil
}

// Names returns every registered provider name in registration order.
// Used by `llm provider list` and by error messages that
// suggest the registered set.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.builders))
	for n := range r.builders {
		out = append(out, n)
	}
	return out
}

// ErrUnknownProvider is returned by Get when the name isn't
// registered.
var ErrUnknownProvider = errors.New("llmprovider: unknown provider")

// DefaultRegistry is the package-level registry every in-tree
// provider self-registers against.
var DefaultRegistry = NewRegistry()
