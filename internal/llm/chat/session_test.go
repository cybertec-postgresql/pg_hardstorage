package chat

import (
	"context"
	"errors"
	stdfs "io/fs"
	"iter"
	"strings"
	"sync"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/skills"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/tools"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/llmprovider"
)

// scriptedProvider is a tool-aware test provider.  Each scripted
// turn yields either (text + Done) or (one ToolCall + Done); the
// session loop dispatches the tool, echoes the result back, and
// the next scripted turn fires.
type scriptedProvider struct {
	mu       sync.Mutex
	turns    []scriptedTurn
	captured [][]llmprovider.Message
}

type scriptedTurn struct {
	text     string
	toolCall *llmprovider.ToolCallChunk
	usage    llmprovider.Usage
	err      error
}

func (p *scriptedProvider) Name() string                                               { return "scripted" }
func (p *scriptedProvider) Open(_ context.Context, _ llmprovider.ProviderConfig) error { return nil }
func (p *scriptedProvider) SupportsTools() bool                                        { return true }
func (p *scriptedProvider) SupportsStreaming() bool                                    { return true }
func (p *scriptedProvider) Close() error                                               { return nil }

func (p *scriptedProvider) Chat(_ context.Context, msgs []llmprovider.Message, _ []llmprovider.ToolDef) iter.Seq2[llmprovider.Chunk, error] {
	return func(yield func(llmprovider.Chunk, error) bool) {
		p.mu.Lock()
		// Snapshot the message list so tests can assert on it.
		snap := make([]llmprovider.Message, len(msgs))
		copy(snap, msgs)
		p.captured = append(p.captured, snap)
		if len(p.turns) == 0 {
			p.mu.Unlock()
			yield(llmprovider.Chunk{}, errors.New("scriptedProvider: no more turns scripted"))
			return
		}
		t := p.turns[0]
		p.turns = p.turns[1:]
		p.mu.Unlock()
		if t.err != nil {
			yield(llmprovider.Chunk{}, t.err)
			return
		}
		if t.text != "" {
			if !yield(llmprovider.Chunk{Text: t.text}, nil) {
				return
			}
		}
		if t.toolCall != nil {
			if !yield(llmprovider.Chunk{ToolCall: t.toolCall}, nil) {
				return
			}
		}
		yield(llmprovider.Chunk{Done: true, Usage: &t.usage}, nil)
	}
}

// fakeTool is a deterministic tool for orchestrator tests.
type fakeTool struct {
	name     string
	desc     string
	readOnly bool
	runFn    func(map[string]any) (tools.Result, error)
}

func (f *fakeTool) Name() string           { return f.name }
func (f *fakeTool) Description() string    { return f.desc }
func (f *fakeTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (f *fakeTool) ReadOnly() bool         { return f.readOnly }
func (f *fakeTool) Run(_ context.Context, args map[string]any) (tools.Result, error) {
	if f.runFn != nil {
		return f.runFn(args)
	}
	return tools.Result{Summary: "ok"}, nil
}

func TestSession_BootstrapValidatesRequired(t *testing.T) {
	if err := (&Session{}).Bootstrap(context.Background()); err == nil {
		t.Error("missing Provider: expected error")
	}
	if err := (&Session{Provider: &scriptedProvider{}}).Bootstrap(context.Background()); err == nil {
		t.Error("missing Tools: expected error")
	}
}

func TestSession_BootstrapAddsSystemMessage(t *testing.T) {
	s := &Session{
		Provider: &scriptedProvider{},
		Tools:    tools.NewRegistry(),
	}
	if err := s.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(s.History) != 1 || s.History[0].Role != "system" {
		t.Fatalf("expected one system message; got %+v", s.History)
	}
	if !strings.Contains(s.History[0].Content, "operator assistant") {
		t.Errorf("system prompt missing default phrasing; got %q", s.History[0].Content)
	}
}

func TestSession_BootstrapIncludesCommandCatalog(t *testing.T) {
	// Prove the CommandCatalog field actually reaches the
	// system prompt — the load-bearing piece for accurate
	// command suggestions.  Without this test, a future
	// refactor could drop the catalog block and the model
	// would silently regress to hallucinating commands.
	s := &Session{
		Provider:       &scriptedProvider{},
		Tools:          tools.NewRegistry(),
		CommandCatalog: "deployment        Manage deployments\n  add             Add a new deployment\n",
	}
	if err := s.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	prompt := s.History[0].Content
	for _, want := range []string{
		"## Command catalog",
		"deployment add",
		"create-style verbs are spelled `add`",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("system prompt missing %q; got %d bytes:\n%s", want, len(prompt), prompt)
		}
	}
}

func TestSession_BootstrapOmitsCatalogWhenEmpty(t *testing.T) {
	// When CommandCatalog is empty (e.g. test that doesn't
	// thread the cobra root through), the catalog block
	// must not be emitted as a sad empty section.
	s := &Session{Provider: &scriptedProvider{}, Tools: tools.NewRegistry()}
	if err := s.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(s.History[0].Content, "## Command catalog") {
		t.Errorf("empty CommandCatalog must not emit the section header")
	}
}

func TestSession_BootstrapIncludesRunbookIndex(t *testing.T) {
	s := &Session{
		Provider: &scriptedProvider{},
		Tools:    tools.NewRegistry(),
	}
	if err := s.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	prompt := s.History[0].Content
	for _, want := range []string{"R1", "R3", "R7"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("system prompt should mention %s; got body of %d bytes", want, len(prompt))
		}
	}
}

func TestSession_AskOneShot(t *testing.T) {
	provider := &scriptedProvider{turns: []scriptedTurn{
		{text: "Hello, tired operator.", usage: llmprovider.Usage{TotalTokens: 12}},
	}}
	s := &Session{
		Provider: provider,
		Tools:    tools.NewRegistry(),
	}
	reply, err := s.Ask(context.Background(), "hi")
	if err != nil {
		t.Fatal(err)
	}
	if reply.Text != "Hello, tired operator." {
		t.Errorf("text = %q", reply.Text)
	}
	if reply.Usage.TotalTokens != 12 {
		t.Errorf("usage = %+v", reply.Usage)
	}
	// History: system, user, assistant.
	if len(s.History) != 3 {
		t.Errorf("history length = %d, want 3", len(s.History))
	}
}

func TestSession_AskWithToolCall(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(&fakeTool{
		name:     "read_doctor",
		desc:     "check health",
		readOnly: true,
		runFn: func(_ map[string]any) (tools.Result, error) {
			return tools.Result{Summary: "all clear", Body: map[string]any{"healthy": true}}, nil
		},
	})
	provider := &scriptedProvider{turns: []scriptedTurn{
		// Turn 1: model requests a tool call.
		{toolCall: &llmprovider.ToolCallChunk{
			ID: "toolu_1", Name: "read_doctor",
			Args: map[string]any{},
		}, usage: llmprovider.Usage{TotalTokens: 5}},
		// Turn 2: model emits the final text answer.
		{text: "Cluster looks healthy.", usage: llmprovider.Usage{TotalTokens: 8}},
	}}
	s := &Session{
		Provider: provider,
		Tools:    reg,
	}
	reply, err := s.Ask(context.Background(), "is it healthy?")
	if err != nil {
		t.Fatal(err)
	}
	if reply.Text != "Cluster looks healthy." {
		t.Errorf("text = %q", reply.Text)
	}
	if len(reply.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(reply.ToolCalls))
	}
	tc := reply.ToolCalls[0]
	if tc.Name != "read_doctor" || tc.Result.Summary != "all clear" {
		t.Errorf("tool invocation = %+v", tc)
	}
	if reply.Usage.TotalTokens != 13 {
		t.Errorf("usage should aggregate across turns; got %+v", reply.Usage)
	}
	// History: system, user, assistant(tool_call), user(tool_result), assistant(text).
	if len(s.History) != 5 {
		t.Fatalf("history length = %d, want 5", len(s.History))
	}
	// The 4th message is the tool_result echo.
	tr := s.History[3]
	if tr.Role != "user" || tr.ToolUseID != "toolu_1" {
		t.Errorf("history[3] should be tool_result echo; got %+v", tr)
	}
	if !strings.Contains(tr.ToolResult, "all clear") {
		t.Errorf("tool result missing summary: %q", tr.ToolResult)
	}
}

func TestSession_Ask_ToolCallBudgetExhausted(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(&fakeTool{name: "read_x", readOnly: true})
	turns := []scriptedTurn{
		{toolCall: &llmprovider.ToolCallChunk{ID: "1", Name: "read_x"}},
		{toolCall: &llmprovider.ToolCallChunk{ID: "2", Name: "read_x"}},
		{toolCall: &llmprovider.ToolCallChunk{ID: "3", Name: "read_x"}},
	}
	s := &Session{
		Provider:            &scriptedProvider{turns: turns},
		Tools:               reg,
		MaxToolCallsPerTurn: 2,
	}
	_, err := s.Ask(context.Background(), "loop")
	if err == nil {
		t.Fatal("expected budget-exhausted error")
	}
	if !strings.Contains(err.Error(), "budget exhausted") {
		t.Errorf("err = %v", err)
	}
}

func TestSession_Ask_TokenBudgetExhausted(t *testing.T) {
	turns := []scriptedTurn{
		{text: "hi", usage: llmprovider.Usage{TotalTokens: 100}},
	}
	s := &Session{
		Provider:                 &scriptedProvider{turns: turns},
		Tools:                    tools.NewRegistry(),
		MaxTokenBudgetPerSession: 50,
	}
	_, err := s.Ask(context.Background(), "first")
	if err != nil {
		t.Fatal(err)
	}
	// Second Ask: usedTokens (100) is already > 50, so the budget
	// gate fires before the second provider call.
	_, err = s.Ask(context.Background(), "second")
	if err == nil {
		t.Fatal("expected token budget exhausted")
	}
	if !strings.Contains(err.Error(), "token budget") {
		t.Errorf("err = %v", err)
	}
}

func TestSession_NonReadOnlyToolRefused(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(&fakeTool{
		name:     "execute_command",
		readOnly: false, // explicitly NOT read-only
		runFn: func(_ map[string]any) (tools.Result, error) {
			t.Fatal("non-read-only tool should not be invoked in v0.5+")
			return tools.Result{}, nil
		},
	})
	provider := &scriptedProvider{turns: []scriptedTurn{
		{toolCall: &llmprovider.ToolCallChunk{ID: "x", Name: "execute_command"}},
		{text: "fine, I'll just suggest it.", usage: llmprovider.Usage{TotalTokens: 1}},
	}}
	s := &Session{
		Provider: provider,
		Tools:    reg,
	}
	reply, err := s.Ask(context.Background(), "do the thing")
	if err != nil {
		t.Fatal(err)
	}
	if len(reply.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d", len(reply.ToolCalls))
	}
	if !strings.Contains(reply.ToolCalls[0].Error, "not read-only") {
		t.Errorf("expected non-read-only refusal in invocation.Error; got %q", reply.ToolCalls[0].Error)
	}
}

func TestSession_AvailableToolsAllowList(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(&fakeTool{name: "read_a", readOnly: true})
	reg.Register(&fakeTool{name: "read_b", readOnly: true})
	reg.Register(&fakeTool{name: "read_c", readOnly: true})
	skill := &skills.Skill{
		Schema: skills.SchemaSkill, Name: "test", Version: "1",
		PromptTemplate: "t",
		Context:        skills.ContextSpec{AvailableTools: []string{"read_a", "read_c"}},
	}
	provider := &scriptedProvider{turns: []scriptedTurn{
		{text: "hi", usage: llmprovider.Usage{TotalTokens: 1}},
	}}
	s := &Session{
		Provider: provider,
		Tools:    reg,
		Skill:    skill,
	}
	if _, err := s.Ask(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	// Inspect the ToolDefs the provider was offered (we can only do
	// this by exposing them through the test provider's Chat).
	defs := s.toolDefsForActiveSkill()
	if len(defs) != 2 {
		t.Fatalf("toolDefs = %d, want 2 (allowlist filter)", len(defs))
	}
	names := map[string]bool{defs[0].Name: true, defs[1].Name: true}
	if !names["read_a"] || !names["read_c"] || names["read_b"] {
		t.Errorf("filter wrong: %+v", names)
	}
}

func TestSession_PreloadToolRunsAtBootstrap(t *testing.T) {
	reg := tools.NewRegistry()
	var called int
	reg.Register(&fakeTool{
		name: "read_doctor", readOnly: true,
		runFn: func(_ map[string]any) (tools.Result, error) {
			called++
			return tools.Result{Summary: "preloaded", Body: map[string]any{"x": 1}}, nil
		},
	})
	skill := &skills.Skill{
		Schema: skills.SchemaSkill, Name: "test", Version: "1",
		PromptTemplate: "doctor preloaded:",
		Context: skills.ContextSpec{
			PreloadTools: []skills.ToolPreload{{Name: "read_doctor"}},
		},
	}
	s := &Session{
		Provider: &scriptedProvider{},
		Tools:    reg,
		Skill:    skill,
	}
	if err := s.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if called != 1 {
		t.Errorf("preload tool called %d times; want 1", called)
	}
	if !strings.Contains(s.History[0].Content, "preloaded") {
		t.Errorf("preload result missing from system prompt; got %q", s.History[0].Content)
	}
}

func TestSession_PreloadToolFailureDegradesGracefully(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(&fakeTool{
		name: "broken_tool", readOnly: true,
		runFn: func(_ map[string]any) (tools.Result, error) {
			return tools.Result{}, errors.New("simulated failure")
		},
	})
	skill := &skills.Skill{
		Schema: skills.SchemaSkill, Name: "test", Version: "1",
		PromptTemplate: "x",
		Context: skills.ContextSpec{
			PreloadTools: []skills.ToolPreload{{Name: "broken_tool"}},
		},
	}
	s := &Session{
		Provider: &scriptedProvider{},
		Tools:    reg,
		Skill:    skill,
	}
	if err := s.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.History[0].Content, "simulated failure") {
		t.Errorf("preload failure should be recorded in prompt; got %q", s.History[0].Content)
	}
}

func TestSession_SnapshotContext(t *testing.T) {
	skill := &skills.Skill{Name: "ask", Version: "1.0.0"}
	s := &Session{
		Provider:                 &scriptedProvider{},
		Tools:                    tools.NewRegistry(),
		Skill:                    skill,
		MaxTokenBudgetPerSession: 1000,
	}
	if err := s.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	snap := s.SnapshotContext()
	if snap["skill"] != "ask" || snap["skill_version"] != "1.0.0" {
		t.Errorf("snapshot skill fields wrong: %+v", snap)
	}
	if snap["provider"] != "scripted" {
		t.Errorf("snapshot provider = %v", snap["provider"])
	}
	if snap["token_budget"].(int) != 1000 {
		t.Errorf("token_budget = %v", snap["token_budget"])
	}
}

// keep the io/fs import live for future tests.
var _ = stdfs.ErrNotExist
