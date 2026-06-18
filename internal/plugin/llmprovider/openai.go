// openai.go — OpenAIProvider: Chat Completions client (also drives OpenAI-compatible endpoints).
package llmprovider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"strings"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/airgap"
)

func init() {
	DefaultRegistry.Register("openai", func() Provider { return &OpenAIProvider{} })
}

// OpenAIEndpoint is the canonical OpenAI Chat Completions API
// host.  Override via ProviderConfig.Endpoint to point the same
// provider at any OpenAI-compatible service:
//
//   - api.openai.com (the default)
//   - Azure OpenAI                — https://<resource>.openai.azure.com/openai/deployments/<dep>
//   - Ollama (local)              — http://127.0.0.1:11434/v1
//   - LiteLLM / OpenRouter        — https://openrouter.ai/api/v1
//   - Together / Groq / Fireworks — https://api.together.xyz/v1, etc.
//   - vLLM / lmdeploy / llama.cpp server — wherever they're hosted
//
// The provider speaks the OpenAI Chat Completions wire format —
// the de-facto industry standard for chat-with-tools.  Routing
// every backend through it means one wire format to maintain,
// one set of tests, and air-gapped deployments work the same way
// (point at a local Ollama or vLLM endpoint).
const OpenAIEndpoint = "https://api.openai.com"

// DefaultOpenAIModel is the model used when ProviderConfig.Model
// is empty.  gpt-4o-mini is the right balance of capability and
// cost for the operator-assistant use case; operators can
// override via config.
const DefaultOpenAIModel = "gpt-4o-mini"

// DefaultOpenAIMaxTokens caps the model's response length.
// 4096 is comfortable for chat answers + a few tool-call cycles
// before the orchestrator has to decide whether to extend.
const DefaultOpenAIMaxTokens = 4096

// OpenAIProvider talks to any OpenAI-compatible Chat Completions
// endpoint with full streaming + tool-use support.  This is the
// production provider for+; the previous Anthropic-native
// and Ollama-native providers were removed in favour of routing
// every backend through this one shape.
//
// What's implemented:
//
//   - Streaming via Server-Sent Events.  Yields one Chunk per
//     assistant text delta and one Chunk per completed tool_use.
//     The final chunk carries Done=true plus the Usage tally
//     (when the upstream reports it; some OpenAI-compatible
//     services omit usage on streaming, in which case the
//     tally remains zero).
//   - Tool use: orchestrator's []ToolDef serialised to OpenAI's
//     `tools: [{type: function, function: {...}}]` shape;
//     accumulated `tool_calls` deltas re-emitted as Chunks with
//     ToolCall populated.
//   - Conversation round-trip: Message.ToolCall renders as an
//     assistant turn with `tool_calls`; Message.ToolUseID +
//     Message.ToolResult render as a `role: "tool"` turn with
//     `tool_call_id` pairing.
//
// What's deliberately NOT implemented in this commit:
//
//   - Prompt caching (OpenAI's `prompt_cache_key` parameter).
//     Useful for the bootstrap prompt; lands in a follow-up
//     once the chat orchestrator surfaces the cache key.
//   - Vision / image input.  Out of scope for the operator
//     assistant.
//   - Structured outputs / response_format JSON schema.  Useful
//     for a future "extract a typed answer" skill; not needed
//     for read-only.
//   - Function calling parallelism control.  We accept whatever
//     the model returns (single tool per turn, or many).
type OpenAIProvider struct {
	cfg    ProviderConfig
	client *http.Client
}

// Name implements Provider.
func (p *OpenAIProvider) Name() string { return "openai" }

// Open implements Provider.  Validates that an API key is present
// (mandatory for the canonical OpenAI endpoint; optional for
// local-network endpoints like Ollama where the key isn't
// authenticated — operators set APIKey to any non-empty
// placeholder in that case, or leave it empty if the endpoint
// genuinely doesn't need one).
//
// Air-gap: if the process-wide air-gap policy is in strict mode,
// the configured Endpoint is gated through airgap.Default()
// before the client is built — a public endpoint refused here
// fails fast with a wrapped airgap.ErrEndpointNotAllowed instead
// of silently producing one TCP connection per Chat call.
func (p *OpenAIProvider) Open(_ context.Context, cfg ProviderConfig) error {
	if cfg.Endpoint == "" {
		cfg.Endpoint = OpenAIEndpoint
	}
	if cfg.Model == "" {
		cfg.Model = DefaultOpenAIModel
	}
	if err := airgap.Default().EndpointAllowed(cfg.Endpoint); err != nil {
		return fmt.Errorf("openai: %w", err)
	}
	// Local-network endpoints (Ollama / vLLM / LM Studio) often
	// don't authenticate — we don't refuse those configurations,
	// but the canonical api.openai.com host MUST have a key.
	if cfg.APIKey == "" && isPublicOpenAIEndpoint(cfg.Endpoint) {
		return errors.New("openai: APIKey is required for the canonical OpenAI endpoint (set OPENAI_API_KEY or configure llm.api_key_file in pg_hardstorage.yaml). For local Ollama / vLLM you can leave it empty or set any placeholder.")
	}
	p.cfg = cfg
	p.client = &http.Client{
		// Generous overall timeout — a long completion + tool use
		// can take a couple of minutes.  ctx cancellation is the
		// authoritative early-exit; this is just a backstop for
		// hung connections.
		Timeout: 5 * time.Minute,
	}
	return nil
}

// Chat implements Provider.  Streams Server-Sent Events from the
// /v1/chat/completions endpoint, translating each event into a
// Chunk.
func (p *OpenAIProvider) Chat(ctx context.Context, msgs []Message, tools []ToolDef) iter.Seq2[Chunk, error] {
	return func(yield func(Chunk, error) bool) {
		body, err := p.buildRequest(msgs, tools)
		if err != nil {
			yield(Chunk{}, err)
			return
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.chatCompletionsURL(), bytes.NewReader(body))
		if err != nil {
			yield(Chunk{}, fmt.Errorf("openai: build request: %w", err))
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")
		if p.cfg.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
		}
		// Azure OpenAI uses a different auth header (`api-key`).
		// Operators pointing at Azure set Extra["api_key_header"]
		// to "api-key" in the config; we honour any custom name.
		if hdr, ok := p.cfg.Extra["api_key_header"].(string); ok && hdr != "" && p.cfg.APIKey != "" {
			req.Header.Del("Authorization")
			req.Header.Set(hdr, p.cfg.APIKey)
		}

		resp, err := p.client.Do(req)
		if err != nil {
			yield(Chunk{}, fmt.Errorf("openai: post: %w", err))
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			rawBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
			yield(Chunk{}, parseOpenAIError(resp.StatusCode, rawBody, p.cfg.Model))
			return
		}

		streamOpenAISSE(ctx, resp.Body, yield)
	}
}

// SupportsTools implements Provider.  OpenAI's Chat Completions
// API has first-class function calling, and every OpenAI-compat
// endpoint we route through (Ollama /v1, vLLM, OpenRouter,
// Azure) also supports it.  Some smaller models silently drop
// tool support; that's a model issue, not a provider issue.
func (p *OpenAIProvider) SupportsTools() bool { return true }

// SupportsStreaming implements Provider.
func (p *OpenAIProvider) SupportsStreaming() bool { return true }

// Close implements Provider.
func (p *OpenAIProvider) Close() error { return nil }

// chatCompletionsURL builds the full URL for the chat
// completions endpoint, honouring the convention that the
// endpoint already ends with `/v1` (Ollama, OpenRouter,
// LiteLLM) vs the canonical OpenAI host that doesn't.
func (p *OpenAIProvider) chatCompletionsURL() string {
	base := strings.TrimRight(p.cfg.Endpoint, "/")
	if strings.HasSuffix(base, "/v1") {
		return base + "/chat/completions"
	}
	return base + "/v1/chat/completions"
}

// buildRequest renders the /v1/chat/completions body.  Pure
// OpenAI shape: messages array (no system-lifting), tools as
// {type: function, function: {...}}, stream=true.
func (p *OpenAIProvider) buildRequest(msgs []Message, tools []ToolDef) ([]byte, error) {
	if p.cfg.Model == "" {
		return nil, errors.New("openai: empty Model")
	}

	wireMsgs := make([]openaiMessage, 0, len(msgs))
	for _, m := range msgs {
		wireMsgs = append(wireMsgs, renderOpenAIMessage(m))
	}

	maxTokens := DefaultOpenAIMaxTokens
	switch v := p.cfg.Extra["max_tokens"].(type) {
	case int:
		if v > 0 {
			maxTokens = v
		}
	case float64:
		if v > 0 {
			maxTokens = int(v)
		}
	}

	// Temperature is optional.  Unset → omit from the wire request
	// (server-side default applies).  Set via Extra["temperature"]
	// in the operator's LLM config or via PG_HARDSTORAGE_LLM_TEMPERATURE
	// env var.  Use 0.0 for deterministic outputs (testkit / CI),
	// raise toward 0.7 for varied prose.
	var temperature *float64
	switch v := p.cfg.Extra["temperature"].(type) {
	case float64:
		t := v
		temperature = &t
	case int:
		t := float64(v)
		temperature = &t
	}

	out := openaiRequest{
		Model:       p.cfg.Model,
		Messages:    wireMsgs,
		Stream:      true,
		MaxTokens:   maxTokens,
		Temperature: temperature,
		// stream_options.include_usage asks compatible servers to
		// emit a usage tally on the final delta.  Servers that
		// don't recognise it ignore it (we then get zero usage,
		// which is acceptable degradation).
		StreamOptions: &openaiStreamOptions{IncludeUsage: true},
	}
	if len(tools) > 0 {
		out.Tools = renderOpenAITools(tools)
	}
	return json.Marshal(out)
}

// renderOpenAIMessage builds an OpenAI chat message from the
// orchestrator's typed Message.
//
// Three shapes:
//   - role=tool:      tool_call_id + content (echoing a tool result).
//   - role=assistant w/ ToolCall: assistant message that
//     invoked a tool (content possibly empty).
//   - everything else: plain text content (system / user /
//     assistant text).
func renderOpenAIMessage(m Message) openaiMessage {
	if m.ToolUseID != "" {
		return openaiMessage{
			Role:       "tool",
			ToolCallID: m.ToolUseID,
			Content:    m.ToolResult,
		}
	}
	if m.Role == "assistant" && m.ToolCall != nil {
		argsBytes, _ := json.Marshal(m.ToolCall.Args)
		return openaiMessage{
			Role:    "assistant",
			Content: m.Content, // may be empty
			ToolCalls: []openaiToolCall{{
				ID:   m.ToolCall.ID,
				Type: "function",
				Function: openaiFunctionCall{
					Name:      m.ToolCall.Name,
					Arguments: string(argsBytes),
				},
			}},
		}
	}
	return openaiMessage{
		Role:    m.Role,
		Content: m.Content,
	}
}

// renderOpenAITools translates ToolDefs into OpenAI's nested
// shape: {type: function, function: {name, description,
// parameters}}.  `parameters` is the JSON Schema we already
// stored in ToolDef.Schema.
func renderOpenAITools(tools []ToolDef) []openaiTool {
	out := make([]openaiTool, 0, len(tools))
	for _, t := range tools {
		out = append(out, openaiTool{
			Type: "function",
			Function: openaiToolFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Schema,
			},
		})
	}
	return out
}

// streamOpenAISSE reads the streamed Server-Sent Events from rd
// and translates them into Chunks via yield.  Returns when the
// stream completes normally, the context is cancelled, or yield
// returns false.
//
// OpenAI's wire format:
//
//	data: {"choices":[{"delta":{"content":"Hello"}}]}\n\n
//	data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"...","function":{"name":"x","arguments":"{"}}]}}]}\n\n
//	data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"deployment\":"}}]}}]}\n\n
//	...
//	data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{...}}\n\n
//	data: [DONE]\n\n
//
// We accumulate per-tool-call state across deltas (id +
// function.name + concatenated arguments) and emit a Chunk with
// ToolCall set when finish_reason fires.  Text content
// streams through directly.
func streamOpenAISSE(ctx context.Context, rd io.Reader, yield func(Chunk, error) bool) {
	sc := bufio.NewScanner(rd)
	sc.Buffer(make([]byte, 64*1024), 1<<20) // 1 MiB max line

	// Some models (DeepSeek-R1, Claude reasoning, several
	// open-weights tunes) embed chain-of-thought inline in
	// `content` wrapped in <think>...</think> or
	// <thinking>...</thinking> tags.  Strip them so the
	// operator sees only the final answer.  The filter
	// buffers across stream chunks because tag boundaries
	// can land mid-token.  See thinkFilter below.
	think := &thinkFilter{}

	// Per-index tool-call accumulator.  Index → id+name+args buffer.
	type toolAcc struct {
		id      string
		name    string
		argsBuf strings.Builder
	}
	tools := map[int]*toolAcc{}
	var (
		usage Usage
		done  bool
	)

	flushToolCalls := func() bool {
		// Emit accumulated tool calls in index order.
		// OpenAI doesn't guarantee index order across deltas,
		// so we sort.
		indexes := make([]int, 0, len(tools))
		for idx := range tools {
			indexes = append(indexes, idx)
		}
		// In-place insertion sort (n is tiny).
		for i := 1; i < len(indexes); i++ {
			for j := i; j > 0 && indexes[j-1] > indexes[j]; j-- {
				indexes[j-1], indexes[j] = indexes[j], indexes[j-1]
			}
		}
		for _, idx := range indexes {
			acc := tools[idx]
			args := map[string]any{}
			if s := strings.TrimSpace(acc.argsBuf.String()); s != "" {
				if err := json.Unmarshal([]byte(s), &args); err != nil {
					if !yield(Chunk{}, fmt.Errorf("openai: parse tool_call arguments: %w (raw=%q)", err, s)) {
						return false
					}
					continue
				}
			}
			if !yield(Chunk{ToolCall: &ToolCallChunk{
				ID:   acc.id,
				Name: acc.name,
				Args: args,
			}}, nil) {
				return false
			}
		}
		return true
	}

	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			yield(Chunk{}, err)
			return
		}
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			done = true
			break
		}

		var delta openaiStreamLine
		if err := json.Unmarshal([]byte(payload), &delta); err != nil {
			yield(Chunk{}, fmt.Errorf("openai: parse stream line: %w (raw=%q)", err, payload))
			return
		}

		// Usage is on the FINAL message only when stream_options
		// include_usage is set; some servers put it on every line
		// (zero on intermediate deltas).  We take the highest values
		// seen so a non-zero final wins over zero placeholders.
		if delta.Usage != nil {
			if delta.Usage.PromptTokens > usage.PromptTokens {
				usage.PromptTokens = delta.Usage.PromptTokens
			}
			if delta.Usage.CompletionTokens > usage.CompletionTokens {
				usage.CompletionTokens = delta.Usage.CompletionTokens
			}
			if delta.Usage.TotalTokens > usage.TotalTokens {
				usage.TotalTokens = delta.Usage.TotalTokens
			}
		}

		// Process choice deltas.
		for _, choice := range delta.Choices {
			// Drop any reasoning_content emitted as a
			// separate field — see ReasoningContent's
			// docstring for why.  An aborted return
			// here would stop the whole stream; we just
			// don't yield it.
			_ = choice.Delta.ReasoningContent

			if choice.Delta.Content != "" {
				visible := think.push(choice.Delta.Content)
				if visible != "" {
					if !yield(Chunk{Text: visible}, nil) {
						return
					}
				}
			}
			for _, tc := range choice.Delta.ToolCalls {
				acc := tools[tc.Index]
				if acc == nil {
					acc = &toolAcc{}
					tools[tc.Index] = acc
				}
				if tc.ID != "" {
					acc.id = tc.ID
				}
				if tc.Function.Name != "" {
					acc.name = tc.Function.Name
				}
				acc.argsBuf.WriteString(tc.Function.Arguments)
			}
			// finish_reason "tool_calls" or "stop" ends the response.
			if choice.FinishReason != "" {
				if len(tools) > 0 {
					if !flushToolCalls() {
						return
					}
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		yield(Chunk{}, fmt.Errorf("openai: read stream: %w", err))
		return
	}
	// Edge case: stream ended without a [DONE] sentinel but the
	// connection closed cleanly.  Flush tool calls (if any), then
	// emit the final Done chunk so the orchestrator unblocks.
	if !done && len(tools) > 0 {
		if !flushToolCalls() {
			return
		}
	}
	// Flush any text the think-filter held back — if the
	// stream ended mid-buffer (e.g. the close tag never
	// arrived), we surface what we have rather than
	// silently swallowing a truncated reply.  Outside-tag
	// pending bytes flush as visible text; inside-tag
	// bytes were already discarded by design.
	if tail := think.flush(); tail != "" {
		yield(Chunk{Text: tail}, nil)
	}
	yield(Chunk{Done: true, Usage: &usage}, nil)
}

// thinkFilter strips <think>...</think> and
// <thinking>...</thinking> blocks from a streamed content
// channel, and also drops everything before an ORPHAN
// </think> close tag (chain-of-thought emitted by a model
// that forgot the opener — e.g. when the upstream API put
// the open marker on a separate `reasoning_content` field
// but echoed the closer in `content`, or the model just
// hallucinated it).
//
// Output strategy: the filter buffers all "outside" text
// internally and only releases it via flush() at end of
// stream.  This is what makes the orphan-close fix possible
// — once we've yielded text downstream we can't take it
// back, so we hold it until we know there's no </think>
// coming that would retroactively mark it as reasoning.
// Chat sessions accumulate the full reply before printing,
// so this batching has no UX impact.
//
// State machine:
//
//   - Outside a thinking block: accumulate visible bytes in
//     visibleBuf.  If a stray `</think>` shows up before
//     any open tag, clear visibleBuf (everything before was
//     unannounced reasoning).  Hold trailing partial-tag
//     bytes (open OR close) in pending until the next push
//     resolves them.
//   - Inside a thinking block: drop content; hold trailing
//     `</...` bytes pending until either the close tag
//     completes (skip + exit) or a non-close character
//     proves they're not the close tag (drop, stay inside).
//
// Pending is bounded by the longest tag (`</thinking>` =
// 11 chars) so memory is trivially flat.  visibleBuf grows
// with the answer length, same as the chat session's own
// accumulator.
type thinkFilter struct {
	inside     bool
	pending    string // partial tag bytes held across pushes
	visibleBuf string // accumulated outside-state visible text
}

const (
	thinkOpen1  = "<think>"
	thinkOpen2  = "<thinking>"
	thinkClose1 = "</think>"
	thinkClose2 = "</thinking>"
)

// push consumes more streamed content.  Always returns ""
// — visible text is held in visibleBuf and released by
// flush() at end of stream.  The early-release contract
// from the design was incompatible with orphan-close
// detection (you can't unsend bytes you've already yielded).
func (f *thinkFilter) push(text string) string {
	combined := f.pending + text
	for {
		if f.inside {
			idx, tagLen := findEither(combined, thinkClose1, thinkClose2)
			if idx < 0 {
				// No close tag yet.  Drop everything we can,
				// keep the trailing bytes that might be the
				// start of a close tag.
				keep := pendingPrefixLen(combined, thinkClose1, thinkClose2)
				f.pending = combined[len(combined)-keep:]
				return ""
			}
			f.inside = false
			combined = combined[idx+tagLen:]
			continue
		}

		// Outside.  Find the next tag — open or close.  Whichever
		// comes first determines the action.
		idxOpen, openLen := findEither(combined, thinkOpen1, thinkOpen2)
		idxClose, closeLen := findEither(combined, thinkClose1, thinkClose2)
		if idxClose >= 0 && (idxOpen < 0 || idxClose < idxOpen) {
			// Orphan close (no preceding open in this stream).
			// Treat everything before it — including anything
			// previously buffered in this stream — as reasoning
			// the model emitted without an opener.  Drop it.
			f.visibleBuf = ""
			combined = combined[idxClose+closeLen:]
			continue
		}
		if idxOpen < 0 {
			// No tag in sight.  Emit what we can to visibleBuf;
			// hold the trailing bytes that might be the start of
			// either an open or close tag.
			keep := pendingPrefixLen(combined, thinkOpen1, thinkOpen2)
			if k := pendingPrefixLen(combined, thinkClose1, thinkClose2); k > keep {
				keep = k
			}
			f.visibleBuf += combined[:len(combined)-keep]
			f.pending = combined[len(combined)-keep:]
			return ""
		}
		// Open tag complete.  Append the prefix to visibleBuf,
		// switch to inside mode, keep scanning.
		f.visibleBuf += combined[:idxOpen]
		f.inside = true
		combined = combined[idxOpen+openLen:]
	}
}

// flush is called at end-of-stream.  Releases the buffered
// outside-state visible text.  Pending tail bytes from
// outside state were never the start of a tag (the stream
// is over), so we surface them as visible.  Inside-state
// pending bytes were chain-of-thought that never closed;
// drop them.
func (f *thinkFilter) flush() string {
	if !f.inside {
		f.visibleBuf += f.pending
	}
	out := f.visibleBuf
	f.visibleBuf = ""
	f.pending = ""
	f.inside = false
	return out
}

// findEither returns (index, length) of the first
// occurrence of either needle in haystack, or (-1, 0) when
// neither matches.  We prefer the LONGER tag when both
// match starting at the same index (rare; covers the
// pathological `<thinking>` vs `<think>` overlap by
// returning the longer one).
func findEither(haystack, a, b string) (int, int) {
	idxA := strings.Index(haystack, a)
	idxB := strings.Index(haystack, b)
	switch {
	case idxA < 0 && idxB < 0:
		return -1, 0
	case idxA < 0:
		return idxB, len(b)
	case idxB < 0:
		return idxA, len(a)
	case idxA < idxB:
		return idxA, len(a)
	case idxB < idxA:
		return idxB, len(b)
	default:
		// Tied — pick the longer match so we consume the
		// wider tag.
		if len(a) >= len(b) {
			return idxA, len(a)
		}
		return idxB, len(b)
	}
}

// pendingPrefixLen returns the number of trailing bytes of
// haystack that COULD be the start of either needle.  Used
// by the filter to decide how many bytes to hold back when
// the outer search returned no full match.
func pendingPrefixLen(haystack, a, b string) int {
	max := pendingPrefixLenFor(haystack, a)
	if k := pendingPrefixLenFor(haystack, b); k > max {
		max = k
	}
	return max
}

func pendingPrefixLenFor(haystack, needle string) int {
	// Largest k > 0 such that needle starts with
	// haystack's last k bytes.  Bounded by len(needle)-1
	// (a full match would have been caught upstream).
	maxK := len(needle) - 1
	if maxK > len(haystack) {
		maxK = len(haystack)
	}
	for k := maxK; k > 0; k-- {
		if strings.HasPrefix(needle, haystack[len(haystack)-k:]) {
			return k
		}
	}
	return 0
}

// parseOpenAIError returns a structured error for a non-2xx
// response.  We surface the API's typed `error.type` +
// `error.message` when the body is parseable so the operator
// sees "invalid_api_key" rather than "status 401".
//
// activeModel is the model name in the request — appended to
// the model-not-found error path so the operator sees both
// the configured model AND how to change it (env var / flag /
// yaml).  Without that hint, the upstream's bare "Model with
// name X does not exist" leaves the operator wondering whose
// expectation X comes from.
func parseOpenAIError(status int, body []byte, activeModel string) error {
	var resp struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	parsed := json.Unmarshal(body, &resp) == nil && resp.Error.Message != ""

	// Model-not-found: 404 + the upstream's message contains
	// "model" (case-insensitive).  Detection is heuristic
	// because every OpenAI-compat backend phrases it
	// slightly differently — OpenAI says "The model `X` does
	// not exist", Anthropic via OpenAI-compat says "model:
	// `X` not found", Ollama says "model 'X' not found".
	// All include the substring "model" in the message body.
	bodyText := string(body)
	if parsed {
		bodyText = resp.Error.Message
	}
	lower := strings.ToLower(bodyText)
	if status == 404 && strings.Contains(lower, "model") {
		hint := fmt.Sprintf(
			" — model %q not available at this endpoint; "+
				"set PG_HARDSTORAGE_LLM_MODEL to a model the endpoint exposes "+
				"(or pass --model on the CLI). Examples: openai → gpt-4o-mini / gpt-4o; "+
				"anthropic-via-compat → claude-3-5-sonnet-20241022; "+
				"ollama → whatever you've pulled (llama3.1:8b, mistral, ...)",
			activeModel)
		return fmt.Errorf("openai: status %d: %s%s", status, strings.TrimSpace(bodyText), hint)
	}

	if parsed {
		typ := resp.Error.Type
		if typ == "" {
			typ = resp.Error.Code
		}
		if typ == "" {
			typ = fmt.Sprintf("http_%d", status)
		}
		return fmt.Errorf("openai: status %d (%s): %s", status, typ, resp.Error.Message)
	}
	return fmt.Errorf("openai: status %d: %s", status, strings.TrimSpace(string(body)))
}

// isPublicOpenAIEndpoint reports whether url points at a host
// that requires a real OpenAI API key.  Conservative: any host
// that isn't 127.0.0.1 / localhost / a private-IP range is
// treated as "public" and the missing-key check fires.
func isPublicOpenAIEndpoint(url string) bool {
	low := strings.ToLower(url)
	if strings.Contains(low, "127.0.0.1") || strings.Contains(low, "localhost") ||
		strings.Contains(low, "://10.") || strings.Contains(low, "://192.168.") ||
		strings.Contains(low, "://172.16.") || strings.Contains(low, "://172.17.") ||
		strings.Contains(low, "://172.18.") || strings.Contains(low, "://172.19.") ||
		strings.Contains(low, "://172.2") || strings.Contains(low, "://172.30.") ||
		strings.Contains(low, "://172.31.") {
		return false
	}
	return true
}

// --- wire types ------------------------------------------------------

type openaiRequest struct {
	Model         string               `json:"model"`
	Messages      []openaiMessage      `json:"messages"`
	Stream        bool                 `json:"stream"`
	MaxTokens     int                  `json:"max_tokens,omitempty"`
	Temperature   *float64             `json:"temperature,omitempty"`
	Tools         []openaiTool         `json:"tools,omitempty"`
	StreamOptions *openaiStreamOptions `json:"stream_options,omitempty"`
}

type openaiStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type openaiMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content,omitempty"`
	Name       string           `json:"name,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	ToolCalls  []openaiToolCall `json:"tool_calls,omitempty"`
}

type openaiToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function openaiFunctionCall `json:"function"`
}

type openaiFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type openaiTool struct {
	Type     string             `json:"type"`
	Function openaiToolFunction `json:"function"`
}

type openaiToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type openaiStreamLine struct {
	Choices []openaiChoice `json:"choices"`
	Usage   *openaiUsage   `json:"usage,omitempty"`
}

type openaiChoice struct {
	Index        int         `json:"index"`
	Delta        openaiDelta `json:"delta"`
	FinishReason string      `json:"finish_reason,omitempty"`
}

type openaiDelta struct {
	Role      string                `json:"role,omitempty"`
	Content   string                `json:"content,omitempty"`
	ToolCalls []openaiToolCallDelta `json:"tool_calls,omitempty"`

	// ReasoningContent is the chain-of-thought stream
	// reasoning models emit alongside the main `content`
	// (OpenAI o1/o3/o4, DeepSeek-R1, Gemini-think).
	// We deserialize it so it doesn't end up in the
	// "unknown field" silence-bucket — but we deliberately
	// DROP it on the floor: the operator-assistant UX
	// surfaces final answers + cited tool calls, never the
	// model's internal monologue.  An operator who wants
	// the reasoning trace can switch to an explicit
	// reasoning-aware tool.
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

type openaiToolCallDelta struct {
	Index    int                     `json:"index"`
	ID       string                  `json:"id,omitempty"`
	Type     string                  `json:"type,omitempty"`
	Function openaiFunctionCallDelta `json:"function,omitempty"`
}

type openaiFunctionCallDelta struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type openaiUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}
