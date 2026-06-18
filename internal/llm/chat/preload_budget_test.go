package chat

import (
	"context"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/skills"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/tools"
)

// bigPreloadTool is a stub preload tool that returns a large body — the
// shape that, unbounded, overflowed the provider context window (F1).
type bigPreloadTool struct{ body string }

func (bigPreloadTool) Name() string           { return "read_doctor" }
func (bigPreloadTool) Description() string    { return "stub doctor" }
func (bigPreloadTool) ReadOnly() bool         { return true }
func (bigPreloadTool) Schema() map[string]any { return map[string]any{} }
func (b bigPreloadTool) Run(_ context.Context, _ map[string]any) (tools.Result, error) {
	return tools.Result{Summary: "huge", Body: b.body}, nil
}

// TestRunPreload_CapsHugeOutput is the F1 regression: a 500 KB preload body
// must be truncated to the per-tool budget so the assembled prompt stays
// bounded (the incident skill blew past the model's 196k-token window).
func TestRunPreload_CapsHugeOutput(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(bigPreloadTool{body: strings.Repeat("X", 500_000)})
	sk := &skills.Skill{
		Context: skills.ContextSpec{
			PreloadTools: []skills.ToolPreload{{Name: "read_doctor"}},
		},
	}
	s := &Session{Tools: reg, Skill: sk, MaxPreloadBytesPerTool: 4096}

	var b strings.Builder
	s.runPreload(context.Background(), &b)
	out := b.String()

	if len(out) > 20_000 {
		t.Errorf("preload not bounded: %d bytes (cap was 4096/tool)", len(out))
	}
	if !strings.Contains(out, "truncated") {
		t.Errorf("expected a truncation note, got:\n%s", out)
	}
	// The raw 500k body must NOT be present in full.
	if strings.Contains(out, strings.Repeat("X", 100_000)) {
		t.Error("full oversized body leaked into the prompt")
	}
}

// TestRunPreload_SmallOutputUntouched: a small preload body passes through
// unchanged (no spurious truncation note).
func TestRunPreload_SmallOutputUntouched(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(bigPreloadTool{body: "all good"})
	sk := &skills.Skill{Context: skills.ContextSpec{PreloadTools: []skills.ToolPreload{{Name: "read_doctor"}}}}
	s := &Session{Tools: reg, Skill: sk}

	var b strings.Builder
	s.runPreload(context.Background(), &b)
	if strings.Contains(b.String(), "truncated") {
		t.Errorf("small body should not be truncated:\n%s", b.String())
	}
}

func TestTruncateForPrompt(t *testing.T) {
	if s, tr := truncateForPrompt("short", 100); tr || s != "short" {
		t.Errorf("short string should pass through, got (%q, %v)", s, tr)
	}
	big := strings.Repeat("a", 1000)
	s, tr := truncateForPrompt(big, 100)
	if !tr {
		t.Error("long string should report truncated=true")
	}
	if len(s) > 200 {
		t.Errorf("truncated string too long: %d", len(s))
	}
	if !strings.Contains(s, "truncated") {
		t.Errorf("truncation marker missing: %q", s)
	}
}
