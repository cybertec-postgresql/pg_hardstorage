package llmprovider_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/llmprovider"
)

// chatAll drives a full Chat round-trip against a stub SSE body and
// returns the visible text, every error yielded, and whether a Done
// chunk arrived.
func chatAll(t *testing.T, body string) (text string, errs []error, done bool) {
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
			errs = append(errs, err)
			continue
		}
		out.WriteString(ch.Text)
		if ch.Done {
			done = true
		}
	}
	return out.String(), errs, done
}

// Per the SSE spec the space after "data:" is optional; several
// OpenAI-compatible servers omit it. Such lines must not be dropped.
func TestOpenAI_SSEDataWithoutSpace(t *testing.T) {
	body := "data:" + `{"choices":[{"delta":{"content":"Hello "}}]}` + "\n\n" +
		ssel(`{"choices":[{"delta":{"content":"world"}}]}`) +
		"data:" + `{"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		"data:[DONE]\n\n" +
		// Anything after [DONE] must be ignored — proves the
		// no-space sentinel was honoured.
		ssel(`{"choices":[{"delta":{"content":" TRAILING"}}]}`)
	text, errs, done := chatAll(t, body)
	if len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	if text != "Hello world" || !done {
		t.Errorf("text = %q done = %v; want %q, true", text, done, "Hello world")
	}
}

// A mid-stream error event (OpenAI and vLLM send
// `data: {"error":{...}}` after the 200 header is already out) must
// surface as an error, not end as an empty "successful" answer.
func TestOpenAI_SSEMidStreamErrorSurfaced(t *testing.T) {
	for name, payload := range map[string]string{
		"object": `{"error":{"message":"The server had an error while processing your request","type":"server_error","code":null}}`,
		"int":    `{"error":{"message":"overloaded","type":"InternalServerError","code":503},"object":"error"}`,
		"string": `{"error":"model crashed"}`,
	} {
		t.Run(name, func(t *testing.T) {
			body := ssel(`{"choices":[{"delta":{"content":"partial"}}]}`) + ssel(payload)
			_, errs, done := chatAll(t, body)
			if len(errs) == 0 {
				t.Fatalf("mid-stream error event swallowed (done=%v)", done)
			}
			if msg := errs[0].Error(); !strings.HasPrefix(msg, "openai: stream error") {
				t.Errorf("error = %q; want an \"openai: stream error\"", msg)
			}
			if done {
				t.Error("stream reported Done after an error event")
			}
		})
	}
}

// The error's message reaches the operator.
func TestOpenAI_SSEMidStreamErrorMessage(t *testing.T) {
	body := ssel(`{"error":{"message":"context length exceeded","type":"invalid_request_error"}}`)
	_, errs, _ := chatAll(t, body)
	if len(errs) == 0 || !strings.Contains(errs[0].Error(), "context length exceeded") ||
		!strings.Contains(errs[0].Error(), "invalid_request_error") {
		t.Fatalf("errs = %v; want the upstream message and type", errs)
	}
}
