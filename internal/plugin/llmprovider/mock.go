// mock.go — MockProvider: in-tree echoing LLM provider for deterministic orchestrator tests.
package llmprovider

import (
	"context"
	"errors"
	"iter"
	"strings"
	"sync"
)

func init() {
	DefaultRegistry.Register("mock", func() Provider { return &MockProvider{} })
}

// MockProvider is the test-time provider. It returns a canned
// completion that echoes the last user message — sufficient for
// orchestrator tests that need a deterministic provider without
// going to the network.
//
// MockProvider is in-tree (not behind a build tag) because tests in
// downstream packages (cli, llm/chat) need it. The cost is one
// always-registered provider; the benefit is no test-only
// dependencies sneaking into builds via go's -tags resolution.
type MockProvider struct {
	mu      sync.Mutex
	cfg     ProviderConfig
	scripts map[string]string // last-user-content → response
}

// Script forces the next Chat call whose last user message contains
// substr to return reply. Multiple calls layer; substring match is
// case-insensitive. Used by tests to assert the orchestrator's
// behavior on specific prompt shapes.
func (m *MockProvider) Script(substr, reply string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.scripts == nil {
		m.scripts = map[string]string{}
	}
	m.scripts[strings.ToLower(substr)] = reply
}

// Name implements Provider.
func (m *MockProvider) Name() string { return "mock" }

// Open implements Provider.
func (m *MockProvider) Open(_ context.Context, cfg ProviderConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfg = cfg
	return nil
}

// Chat implements Provider. Yields a single text chunk + a Done
// chunk with token usage; tests that need streaming behavior get
// it because the orchestrator iterates over both.
func (m *MockProvider) Chat(ctx context.Context, msgs []Message, _ []ToolDef) iter.Seq2[Chunk, error] {
	return func(yield func(Chunk, error) bool) {
		if err := ctx.Err(); err != nil {
			yield(Chunk{}, err)
			return
		}
		reply := m.respond(msgs)
		if !yield(Chunk{Text: reply}, nil) {
			return
		}
		yield(Chunk{
			Done: true,
			Usage: &Usage{
				PromptTokens:     countTokens(msgs),
				CompletionTokens: estimateTokens(reply),
				TotalTokens:      countTokens(msgs) + estimateTokens(reply),
			},
		}, nil)
	}
}

// SupportsTools implements Provider. The mock doesn't call tools.
func (m *MockProvider) SupportsTools() bool { return false }

// SupportsStreaming implements Provider. We yield two chunks (text +
// done), so technically streaming.
func (m *MockProvider) SupportsStreaming() bool { return true }

// Close implements Provider.
func (m *MockProvider) Close() error { return nil }

// respond picks the canned response. If a script matches the last
// user message, return its reply; otherwise return a deterministic
// echo that's adequate for "the orchestrator wires up correctly".
func (m *MockProvider) respond(msgs []Message) string {
	m.mu.Lock()
	defer m.mu.Unlock()

	last := lastUserMessage(msgs)
	if last == "" {
		return "(mock provider: no user message)"
	}
	low := strings.ToLower(last)
	for substr, reply := range m.scripts {
		if strings.Contains(low, substr) {
			return reply
		}
	}
	return "mock-reply: " + last
}

func lastUserMessage(msgs []Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return msgs[i].Content
		}
	}
	return ""
}

// countTokens / estimateTokens: extremely rough heuristic (one token
// ≈ 4 bytes). The mock provider is for plumbing tests, not for
// validating token counts; a precise count would need the provider's
// tokenizer.
func countTokens(msgs []Message) int {
	var total int
	for _, m := range msgs {
		total += estimateTokens(m.Content)
	}
	return total
}

func estimateTokens(s string) int {
	if s == "" {
		return 0
	}
	return (len(s) + 3) / 4
}

// ErrMockNoScript is returned by tests that explicitly want the
// non-script path to error rather than fall back to echo.
var ErrMockNoScript = errors.New("llmprovider: mock has no scripted response for this prompt")
