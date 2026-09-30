package chat

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/privacy"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/skills"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/tools"
)

// leakyPreloadTool returns live-cluster output carrying exactly what the
// privacy modes promise never leaves the host: a connection string with a
// password, an operator email, a private IP.
type leakyPreloadTool struct {
	name string
	err  bool
}

func (l leakyPreloadTool) Name() string         { return l.name }
func (leakyPreloadTool) Description() string    { return "stub" }
func (leakyPreloadTool) ReadOnly() bool         { return true }
func (leakyPreloadTool) Schema() map[string]any { return map[string]any{} }
func (l leakyPreloadTool) Run(_ context.Context, _ map[string]any) (tools.Result, error) {
	if l.err {
		return tools.Result{}, errors.New("exit 1: connect postgres://backup:hunter2@10.1.2.3/db failed for ops@example.com")
	}
	return tools.Result{Summary: "ok", Body: map[string]any{
		"pg_connection": "postgres://backup:hunter2@10.1.2.3:5432/postgres",
		"owner":         "ops@example.com",
		"deployment":    "billing_prod",
	}}, nil
}

// The system prompt embeds raw preload output (read_doctor,
// list_deployments) and the operator's AdditionalContext, and it is
// sent to the provider like any other message.  Privacy modes that
// redact user prompts and tool results but not the system prompt
// leaked the most sensitive block of the whole session.
func TestBootstrap_SystemPromptRedactedPerPrivacyMode(t *testing.T) {
	for _, mode := range []privacy.Mode{privacy.ModeStandard, privacy.ModeStrict, privacy.ModeOpen} {
		reg := tools.NewRegistry()
		reg.Register(leakyPreloadTool{name: "read_doctor"})
		reg.Register(leakyPreloadTool{name: "list_deployments", err: true})
		sk := &skills.Skill{Context: skills.ContextSpec{PreloadTools: []skills.ToolPreload{
			{Name: "read_doctor"}, {Name: "list_deployments"},
		}}}
		s := &Session{
			Provider:          &scriptedProvider{},
			Tools:             reg,
			Skill:             sk,
			Privacy:           mode,
			AdditionalContext: "pager: oncall@example.com, api_key=sk-live-123",
		}
		if err := s.Bootstrap(context.Background()); err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		sys := s.History[0].Content
		// Credentials are masked in every mode, open included.
		for _, secret := range []string{"hunter2", "sk-live-123"} {
			if strings.Contains(sys, secret) {
				t.Errorf("%s: system prompt leaked credential %q", mode, secret)
			}
		}
		if mode == privacy.ModeOpen {
			continue
		}
		for _, pii := range []string{"ops@example.com", "oncall@example.com", "10.1.2.3"} {
			if strings.Contains(sys, pii) {
				t.Errorf("%s: system prompt leaked %q", mode, pii)
			}
		}
		if mode == privacy.ModeStrict && strings.Contains(sys, "billing_prod") {
			t.Errorf("strict: system prompt leaked deployment name")
		}
		// Our own authored prompt text must survive (it is not data).
		if !strings.Contains(sys, "Pre-loaded cluster context") {
			t.Errorf("%s: authored prompt structure was redacted too", mode)
		}
	}
}
