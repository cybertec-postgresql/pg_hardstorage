package llmprovider_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/llmprovider"
)

// stubOpenAIServer captures requests + serves canned SSE bodies.
type stubOpenAIServer struct {
	t        *testing.T
	requests []map[string]any
	headers  []http.Header
	status   int
	body     string
}

func (s *stubOpenAIServer) handler(w http.ResponseWriter, r *http.Request) {
	s.t.Helper()
	if r.Method != http.MethodPost {
		s.t.Errorf("method = %s, want POST", r.Method)
	}
	if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
		s.t.Errorf("path = %s, want suffix /chat/completions", r.URL.Path)
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.t.Fatal(err)
	}
	var dec map[string]any
	if err := json.Unmarshal(body, &dec); err != nil {
		s.t.Fatalf("invalid request JSON: %v\n%s", err, body)
	}
	s.requests = append(s.requests, dec)
	s.headers = append(s.headers, r.Header.Clone())
	if s.status != 0 && s.status != 200 {
		w.WriteHeader(s.status)
		_, _ = w.Write([]byte(s.body))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(s.body))
}

// sseLine helpers — build an OpenAI streaming body.
func ssel(payload string) string { return "data: " + payload + "\n\n" }
func sseDone() string            { return "data: [DONE]\n\n" }

func newOpenAI() llmprovider.Provider {
	p, err := llmprovider.DefaultRegistry.Get("openai")
	if err != nil {
		panic(err)
	}
	return p
}

func TestOpenAI_RequiresKeyForPublicEndpoint(t *testing.T) {
	p := newOpenAI()
	err := p.Open(context.Background(), llmprovider.ProviderConfig{
		Endpoint: "https://api.openai.com",
	})
	if err == nil {
		t.Fatal("expected error: public endpoint without API key")
	}
}

func TestOpenAI_AllowsEmptyKeyForLocalEndpoint(t *testing.T) {
	p := newOpenAI()
	if err := p.Open(context.Background(), llmprovider.ProviderConfig{
		Endpoint: "http://127.0.0.1:11434/v1",
	}); err != nil {
		t.Fatalf("local endpoint without key should be permitted; got %v", err)
	}
}

func TestOpenAI_StreamsTextDeltas(t *testing.T) {
	stub := &stubOpenAIServer{
		t: t,
		body: ssel(`{"choices":[{"delta":{"content":"Hello "}}]}`) +
			ssel(`{"choices":[{"delta":{"content":"world"}}]}`) +
			ssel(`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`) +
			sseDone(),
	}
	srv := httptest.NewServer(http.HandlerFunc(stub.handler))
	defer srv.Close()

	p := newOpenAI()
	if err := p.Open(context.Background(), llmprovider.ProviderConfig{
		APIKey: "sk-test", Endpoint: srv.URL, Model: "gpt-test",
	}); err != nil {
		t.Fatal(err)
	}

	var text strings.Builder
	var done bool
	var usage llmprovider.Usage
	for ch, err := range p.Chat(context.Background(),
		[]llmprovider.Message{{Role: "user", Content: "hi"}}, nil) {
		if err != nil {
			t.Fatalf("chat: %v", err)
		}
		text.WriteString(ch.Text)
		if ch.Done {
			done = true
			if ch.Usage != nil {
				usage = *ch.Usage
			}
		}
	}
	if got := text.String(); got != "Hello world" {
		t.Errorf("text = %q, want %q", got, "Hello world")
	}
	if !done {
		t.Errorf("expected Done=true on the final chunk")
	}
	if usage.PromptTokens != 10 || usage.CompletionTokens != 2 || usage.TotalTokens != 12 {
		t.Errorf("usage = %+v, want (10, 2, 12)", usage)
	}
}

func TestOpenAI_BearerAuthHeader(t *testing.T) {
	stub := &stubOpenAIServer{
		t:    t,
		body: ssel(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`) + sseDone(),
	}
	srv := httptest.NewServer(http.HandlerFunc(stub.handler))
	defer srv.Close()
	p := newOpenAI()
	if err := p.Open(context.Background(), llmprovider.ProviderConfig{
		APIKey: "sk-secret", Endpoint: srv.URL,
	}); err != nil {
		t.Fatal(err)
	}
	for _, err := range p.Chat(context.Background(),
		[]llmprovider.Message{{Role: "user", Content: "hi"}}, nil) {
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(stub.headers) != 1 {
		t.Fatalf("expected 1 request; got %d", len(stub.headers))
	}
	got := stub.headers[0].Get("Authorization")
	if got != "Bearer sk-secret" {
		t.Errorf("Authorization = %q, want Bearer sk-secret", got)
	}
}

func TestOpenAI_AzureCustomAuthHeader(t *testing.T) {
	stub := &stubOpenAIServer{
		t:    t,
		body: ssel(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`) + sseDone(),
	}
	srv := httptest.NewServer(http.HandlerFunc(stub.handler))
	defer srv.Close()
	p := newOpenAI()
	if err := p.Open(context.Background(), llmprovider.ProviderConfig{
		APIKey: "azure-key", Endpoint: srv.URL,
		Extra: map[string]any{"api_key_header": "api-key"},
	}); err != nil {
		t.Fatal(err)
	}
	for _, err := range p.Chat(context.Background(),
		[]llmprovider.Message{{Role: "user", Content: "hi"}}, nil) {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := stub.headers[0].Get("api-key"); got != "azure-key" {
		t.Errorf("api-key header = %q, want azure-key", got)
	}
	// Authorization header should NOT be set when api_key_header is custom.
	if got := stub.headers[0].Get("Authorization"); got != "" {
		t.Errorf("Authorization should be empty under api_key_header override; got %q", got)
	}
}

func TestOpenAI_ToolCallStream(t *testing.T) {
	stub := &stubOpenAIServer{
		t: t,
		body: ssel(`{"choices":[{"delta":{"role":"assistant","content":"Let me check."}}]}`) +
			ssel(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_x","type":"function","function":{"name":"read_doctor","arguments":""}}]}}]}`) +
			ssel(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"deployment\""}}]}}]}`) +
			ssel(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":":\"db1\"}"}}]}}]}`) +
			ssel(`{"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":15,"completion_tokens":20,"total_tokens":35}}`) +
			sseDone(),
	}
	srv := httptest.NewServer(http.HandlerFunc(stub.handler))
	defer srv.Close()
	p := newOpenAI()
	if err := p.Open(context.Background(), llmprovider.ProviderConfig{
		APIKey: "sk-test", Endpoint: srv.URL,
	}); err != nil {
		t.Fatal(err)
	}

	var text strings.Builder
	var sawTool *llmprovider.ToolCallChunk
	for ch, err := range p.Chat(context.Background(),
		[]llmprovider.Message{{Role: "user", Content: "is db1 healthy?"}},
		[]llmprovider.ToolDef{{
			Name: "read_doctor", Description: "check health",
			Schema: map[string]any{"type": "object"},
		}}) {
		if err != nil {
			t.Fatalf("chat: %v", err)
		}
		text.WriteString(ch.Text)
		if ch.ToolCall != nil {
			sawTool = ch.ToolCall
		}
	}
	if text.String() != "Let me check." {
		t.Errorf("text = %q, want 'Let me check.'", text.String())
	}
	if sawTool == nil {
		t.Fatal("expected tool_call")
	}
	if sawTool.ID != "call_x" || sawTool.Name != "read_doctor" {
		t.Errorf("toolcall id/name = (%q, %q), want (call_x, read_doctor)", sawTool.ID, sawTool.Name)
	}
	if dep, _ := sawTool.Args["deployment"].(string); dep != "db1" {
		t.Errorf("toolcall args = %+v, want deployment=db1", sawTool.Args)
	}
}

func TestOpenAI_ConversationRoundTrip(t *testing.T) {
	stub := &stubOpenAIServer{
		t:    t,
		body: ssel(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`) + sseDone(),
	}
	srv := httptest.NewServer(http.HandlerFunc(stub.handler))
	defer srv.Close()
	p := newOpenAI()
	if err := p.Open(context.Background(), llmprovider.ProviderConfig{
		APIKey: "sk-test", Endpoint: srv.URL,
	}); err != nil {
		t.Fatal(err)
	}
	// Conversation: system → user → assistant(tool_call) → tool(result) → assistant
	for _, err := range p.Chat(context.Background(), []llmprovider.Message{
		{Role: "system", Content: "you are an assistant"},
		{Role: "user", Content: "is db1 healthy?"},
		{Role: "assistant", ToolCall: &llmprovider.ToolCallChunk{
			ID: "call_y", Name: "read_doctor",
			Args: map[string]any{"deployment": "db1"},
		}},
		{Role: "user", ToolUseID: "call_y", ToolResult: `{"healthy":true}`},
	}, nil) {
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(stub.requests) != 1 {
		t.Fatalf("expected 1 request; got %d", len(stub.requests))
	}
	msgs, _ := stub.requests[0]["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages count = %d, want 4 (system NOT lifted in OpenAI shape)", len(msgs))
	}
	// 1st message: role=system, content=...
	first := msgs[0].(map[string]any)
	if first["role"] != "system" {
		t.Errorf("first role = %v, want system", first["role"])
	}
	// 3rd message: assistant with tool_calls
	third := msgs[2].(map[string]any)
	if third["role"] != "assistant" {
		t.Errorf("third role = %v", third["role"])
	}
	tcs, ok := third["tool_calls"].([]any)
	if !ok || len(tcs) != 1 {
		t.Fatalf("expected one tool_call on 3rd message; got %+v", third)
	}
	tc := tcs[0].(map[string]any)
	if tc["id"] != "call_y" {
		t.Errorf("tool_call id = %v", tc["id"])
	}
	fn := tc["function"].(map[string]any)
	if fn["name"] != "read_doctor" {
		t.Errorf("function.name = %v", fn["name"])
	}
	// arguments is a JSON-string
	if !strings.Contains(fn["arguments"].(string), "db1") {
		t.Errorf("function.arguments lost the body: %v", fn["arguments"])
	}
	// 4th message: role=tool, tool_call_id=call_y
	fourth := msgs[3].(map[string]any)
	if fourth["role"] != "tool" {
		t.Errorf("fourth role = %v, want tool", fourth["role"])
	}
	if fourth["tool_call_id"] != "call_y" {
		t.Errorf("tool_call_id = %v", fourth["tool_call_id"])
	}
	if !strings.Contains(fourth["content"].(string), "healthy") {
		t.Errorf("tool result content lost: %v", fourth["content"])
	}
}

func TestOpenAI_APIErrorParsed(t *testing.T) {
	stub := &stubOpenAIServer{
		t:      t,
		status: http.StatusUnauthorized,
		body:   `{"error":{"message":"invalid api key","type":"invalid_request_error","code":"invalid_api_key"}}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(stub.handler))
	defer srv.Close()
	p := newOpenAI()
	if err := p.Open(context.Background(), llmprovider.ProviderConfig{
		APIKey: "bad-key", Endpoint: srv.URL,
	}); err != nil {
		t.Fatal(err)
	}
	for _, err := range p.Chat(context.Background(),
		[]llmprovider.Message{{Role: "user", Content: "hi"}}, nil) {
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "invalid_request_error") {
			t.Errorf("err should surface typed code; got %v", err)
		}
		break
	}
}

func TestOpenAI_RegistryHasOpenAI(t *testing.T) {
	p, err := llmprovider.DefaultRegistry.Get("openai")
	if err != nil {
		t.Fatalf("openai provider not in registry: %v", err)
	}
	if p.Name() != "openai" {
		t.Errorf("Name = %q", p.Name())
	}
	if !p.SupportsTools() || !p.SupportsStreaming() {
		t.Error("openai provider should support both tools and streaming")
	}
}

func TestOpenAI_EndpointSlashV1(t *testing.T) {
	// Endpoint that already ends with /v1 (Ollama, OpenRouter) should
	// produce …/v1/chat/completions, NOT …/v1/v1/chat/completions.
	stub := &stubOpenAIServer{
		t:    t,
		body: ssel(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`) + sseDone(),
	}
	srv := httptest.NewServer(http.HandlerFunc(stub.handler))
	defer srv.Close()
	p := newOpenAI()
	if err := p.Open(context.Background(), llmprovider.ProviderConfig{
		APIKey: "x", Endpoint: srv.URL + "/v1",
	}); err != nil {
		t.Fatal(err)
	}
	// Drive one request — the stub asserts the path suffix.
	for _, err := range p.Chat(context.Background(),
		[]llmprovider.Message{{Role: "user", Content: "hi"}}, nil) {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestOpenAI_BuildRequestIncludesTools(t *testing.T) {
	stub := &stubOpenAIServer{
		t:    t,
		body: ssel(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`) + sseDone(),
	}
	srv := httptest.NewServer(http.HandlerFunc(stub.handler))
	defer srv.Close()
	p := newOpenAI()
	if err := p.Open(context.Background(), llmprovider.ProviderConfig{
		APIKey: "x", Endpoint: srv.URL,
	}); err != nil {
		t.Fatal(err)
	}
	for _, err := range p.Chat(context.Background(),
		[]llmprovider.Message{{Role: "user", Content: "hi"}},
		[]llmprovider.ToolDef{
			{Name: "read_doctor", Description: "d", Schema: map[string]any{"type": "object"}},
			{Name: "read_status", Description: "s", Schema: map[string]any{"type": "object"}},
		}) {
		if err != nil {
			t.Fatal(err)
		}
	}
	tools, _ := stub.requests[0]["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools count = %d, want 2", len(tools))
	}
	for _, raw := range tools {
		t0 := raw.(map[string]any)
		if t0["type"] != "function" {
			t.Errorf("tool type = %v, want function", t0["type"])
		}
		if _, ok := t0["function"].(map[string]any); !ok {
			t.Errorf("tool missing function key: %+v", t0)
		}
	}
}

// Smoke test: max_tokens via Extra map flows through.
func TestOpenAI_MaxTokensOverride(t *testing.T) {
	stub := &stubOpenAIServer{
		t:    t,
		body: ssel(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`) + sseDone(),
	}
	srv := httptest.NewServer(http.HandlerFunc(stub.handler))
	defer srv.Close()
	p := newOpenAI()
	if err := p.Open(context.Background(), llmprovider.ProviderConfig{
		APIKey: "x", Endpoint: srv.URL,
		Extra: map[string]any{"max_tokens": 999},
	}); err != nil {
		t.Fatal(err)
	}
	for _, err := range p.Chat(context.Background(),
		[]llmprovider.Message{{Role: "user", Content: "hi"}}, nil) {
		if err != nil {
			t.Fatal(err)
		}
	}
	mt, _ := stub.requests[0]["max_tokens"].(float64)
	if int(mt) != 999 {
		t.Errorf("max_tokens = %v, want 999", mt)
	}
}

// Sanity: `[DONE]` sentinel is handled even without a finish_reason
// on the prior delta.
func TestOpenAI_DoneSentinelTerminates(t *testing.T) {
	stub := &stubOpenAIServer{
		t:    t,
		body: ssel(`{"choices":[{"delta":{"content":"hi"}}]}`) + sseDone(),
	}
	srv := httptest.NewServer(http.HandlerFunc(stub.handler))
	defer srv.Close()
	p := newOpenAI()
	if err := p.Open(context.Background(), llmprovider.ProviderConfig{
		APIKey: "x", Endpoint: srv.URL,
	}); err != nil {
		t.Fatal(err)
	}
	var done bool
	for ch, err := range p.Chat(context.Background(),
		[]llmprovider.Message{{Role: "user", Content: "hi"}}, nil) {
		if err != nil {
			t.Fatal(err)
		}
		if ch.Done {
			done = true
		}
	}
	if !done {
		t.Errorf("expected Done=true after [DONE] sentinel")
	}
}

// Helper to keep the linter from complaining about an unused
// fmt import if a future test removes the only use.
var _ = fmt.Sprintf

// TestOpenAI_ModelNotFound_ActionableError covers the
// model-404 rewrite path: when the upstream returns 404
// with a body mentioning "model" (the way OpenAI / Ollama /
// Anthropic-via-compat all phrase model-not-found), the
// wrapped error MUST include the active model name AND the
// PG_HARDSTORAGE_LLM_MODEL hint so the operator can fix it
// without grepping through docs.
//
// Pre-fix, the operator saw the upstream's bare body
// ("Model with name X does not exist") with no clue whose
// expectation X comes from.  The wrapped error is the
// difference between "what does this mean?" and "I know
// what to type".
func TestOpenAI_ModelNotFound_ActionableError(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			name: "openai-style",
			body: `{"error":{"message":"The model ` + "`gpt-5`" + ` does not exist","type":"invalid_request_error","code":"model_not_found"}}`,
		},
		{
			name: "ollama-style",
			body: `{"error":{"message":"model 'configured-model' not found"}}`,
		},
		{
			name: "raw-text",
			body: "Model with name configured-model does not exist.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &stubOpenAIServer{t: t, status: 404, body: tc.body}
			srv := httptest.NewServer(http.HandlerFunc(stub.handler))
			defer srv.Close()

			p := newOpenAI()
			if err := p.Open(context.Background(), llmprovider.ProviderConfig{
				Endpoint: srv.URL + "/v1",
				APIKey:   "test-key",
				Model:    "configured-model",
			}); err != nil {
				t.Fatal(err)
			}
			defer p.Close()

			var got error
			for _, err := range p.Chat(context.Background(),
				[]llmprovider.Message{{Role: "user", Content: "hi"}}, nil) {
				if err != nil {
					got = err
					break
				}
			}
			if got == nil {
				t.Fatal("expected error from 404 response")
			}
			msg := got.Error()
			if !strings.Contains(msg, `"configured-model"`) {
				t.Errorf("error missing active model hint: %s", msg)
			}
			if !strings.Contains(msg, "PG_HARDSTORAGE_LLM_MODEL") {
				t.Errorf("error missing PG_HARDSTORAGE_LLM_MODEL hint: %s", msg)
			}
		})
	}
}

// TestOpenAI_NonModelError_NoModelHint — a 401 / 429 / 500
// must NOT carry the model-env hint, even if the body
// happens to mention "model" somewhere.  Sanity guard: the
// rewrite path is gated on status==404 specifically.
func TestOpenAI_NonModelError_NoModelHint(t *testing.T) {
	stub := &stubOpenAIServer{
		t:      t,
		status: 401,
		body:   `{"error":{"message":"Incorrect API key for model gpt-4o","type":"invalid_request_error"}}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(stub.handler))
	defer srv.Close()

	p := newOpenAI()
	if err := p.Open(context.Background(), llmprovider.ProviderConfig{
		Endpoint: srv.URL + "/v1",
		APIKey:   "test-key",
		Model:    "configured-model",
	}); err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	var got error
	for _, err := range p.Chat(context.Background(),
		[]llmprovider.Message{{Role: "user", Content: "hi"}}, nil) {
		if err != nil {
			got = err
			break
		}
	}
	if got == nil {
		t.Fatal("expected error from 401 response")
	}
	if strings.Contains(got.Error(), "PG_HARDSTORAGE_LLM_MODEL") {
		t.Errorf("non-404 error should not carry the model-env hint: %s", got)
	}
}

// streamText drives a full Chat round-trip against a stub
// SSE body and returns the visible text the operator would
// see, with the thinking filter applied.
func streamText(t *testing.T, body string) string {
	t.Helper()
	stub := &stubOpenAIServer{t: t, body: body}
	srv := httptest.NewServer(http.HandlerFunc(stub.handler))
	defer srv.Close()
	p := newOpenAI()
	if err := p.Open(context.Background(), llmprovider.ProviderConfig{
		APIKey: "sk-test", Endpoint: srv.URL, Model: "gpt-test",
	}); err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	var out strings.Builder
	for ch, err := range p.Chat(context.Background(),
		[]llmprovider.Message{{Role: "user", Content: "hi"}}, nil) {
		if err != nil {
			t.Fatalf("chat: %v", err)
		}
		out.WriteString(ch.Text)
	}
	return out.String()
}

// Thinking-tag scrubbing.  The provider emits visible
// content only — anything wrapped in <think>...</think> or
// <thinking>...</thinking> never reaches the operator.
// These tests cover the awkward cases where tag boundaries
// land mid-token across stream chunks.

func TestOpenAI_ThinkingTagSingleChunk(t *testing.T) {
	body := ssel(`{"choices":[{"delta":{"content":"Hello <think>internal</think> world"}}]}`) +
		ssel(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`) + sseDone()
	if got := streamText(t, body); got != "Hello  world" {
		t.Errorf("text = %q, want %q", got, "Hello  world")
	}
}

func TestOpenAI_ThinkingTagThinkingVariant(t *testing.T) {
	body := ssel(`{"choices":[{"delta":{"content":"A<thinking>hidden</thinking>B"}}]}`) +
		ssel(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`) + sseDone()
	if got := streamText(t, body); got != "AB" {
		t.Errorf("text = %q, want %q", got, "AB")
	}
}

func TestOpenAI_ThinkingTagSplitOpenAcrossChunks(t *testing.T) {
	// Open tag straddles a chunk boundary: "<thi" / "nk>".
	body := ssel(`{"choices":[{"delta":{"content":"Hello <thi"}}]}`) +
		ssel(`{"choices":[{"delta":{"content":"nk>secret</think> world"}}]}`) +
		ssel(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`) + sseDone()
	if got := streamText(t, body); got != "Hello  world" {
		t.Errorf("text = %q, want %q", got, "Hello  world")
	}
}

func TestOpenAI_ThinkingTagSplitCloseAcrossChunks(t *testing.T) {
	// Close tag straddles a chunk boundary: "</thi" / "nk>".
	body := ssel(`{"choices":[{"delta":{"content":"a<think>x</thi"}}]}`) +
		ssel(`{"choices":[{"delta":{"content":"nk>b"}}]}`) +
		ssel(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`) + sseDone()
	if got := streamText(t, body); got != "ab" {
		t.Errorf("text = %q, want %q", got, "ab")
	}
}

func TestOpenAI_ThinkingTagFalsePositiveLeavesContentIntact(t *testing.T) {
	// "<thinking_aloud>" is not a thinking tag — make sure
	// the filter doesn't swallow it.  The filter must hold
	// `<thinking` pending across chunks, then flush it
	// once `_` proves it's not the close `>` of a real tag.
	body := ssel(`{"choices":[{"delta":{"content":"prefix <thinking_aloud>kept</thinking_aloud> suffix"}}]}`) +
		ssel(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`) + sseDone()
	want := "prefix <thinking_aloud>kept</thinking_aloud> suffix"
	if got := streamText(t, body); got != want {
		t.Errorf("text = %q, want %q", got, want)
	}
}

func TestOpenAI_ThinkingTagUnclosedAtEOSDropped(t *testing.T) {
	// Provider hangs up mid-thinking-block (network blip,
	// upstream cancellation).  Anything after the open tag
	// is chain-of-thought we should drop, even though no
	// close arrived.
	body := ssel(`{"choices":[{"delta":{"content":"visible <think>still-thinking-when"}}]}`) +
		ssel(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`) + sseDone()
	if got := streamText(t, body); got != "visible " {
		t.Errorf("text = %q, want %q", got, "visible ")
	}
}

func TestOpenAI_ReasoningContentFieldDropped(t *testing.T) {
	// OpenAI o1/o3/o4 + DeepSeek-R1 + Gemini-think emit the
	// chain-of-thought on a separate `reasoning_content`
	// field.  The provider must not surface it as visible
	// text.
	body := ssel(`{"choices":[{"delta":{"reasoning_content":"step 1: think","content":"answer"}}]}`) +
		ssel(`{"choices":[{"delta":{"reasoning_content":" step 2"}}]}`) +
		ssel(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`) + sseDone()
	if got := streamText(t, body); got != "answer" {
		t.Errorf("text = %q, want %q", got, "answer")
	}
}

func TestOpenAI_ThinkingTagMultipleBlocks(t *testing.T) {
	// Two blocks, content interleaved.  Each is independent.
	body := ssel(`{"choices":[{"delta":{"content":"a<think>1</think>b<think>2</think>c"}}]}`) +
		ssel(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`) + sseDone()
	if got := streamText(t, body); got != "abc" {
		t.Errorf("text = %q, want %q", got, "abc")
	}
}

func TestOpenAI_ThinkingTagOrphanCloseDropsPrefix(t *testing.T) {
	// The model emits chain-of-thought without an opening
	// tag, then closes with </think>.  Observed in the wild
	// when an OpenAI-compat server moves the open marker to
	// `reasoning_content` but echoes the closer in `content`,
	// or when a model just hallucinates an unbalanced close.
	// Everything before the orphan close is reasoning and
	// must be dropped.
	body := ssel(`{"choices":[{"delta":{"content":"Good, now I need to help them add a deployment.</think>\n\nReal answer."}}]}`) +
		ssel(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`) + sseDone()
	if got := streamText(t, body); got != "\n\nReal answer." {
		t.Errorf("text = %q, want %q", got, "\n\nReal answer.")
	}
}

func TestOpenAI_ThinkingTagOrphanCloseAcrossChunks(t *testing.T) {
	// Orphan close split across stream chunks.  The first
	// chunk's bytes are reasoning prose; the close arrives
	// half in the second chunk.
	body := ssel(`{"choices":[{"delta":{"content":"reasoning text </thi"}}]}`) +
		ssel(`{"choices":[{"delta":{"content":"nk> answer"}}]}`) +
		ssel(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`) + sseDone()
	if got := streamText(t, body); got != " answer" {
		t.Errorf("text = %q, want %q", got, " answer")
	}
}
