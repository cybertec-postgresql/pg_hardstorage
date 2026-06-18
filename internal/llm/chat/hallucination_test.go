package chat

import (
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/cli/cmdtree"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/tools"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/llmprovider"
)

// hallucinationFixtureRoot is a small cobra tree that
// has `deployment add <name>` (the real verb) but NOT
// `deployment create` (the verb the model improvises
// from training data).  Threads through SuggestCommand
// to prove the validator catches the made-up command
// shape end-to-end.
func hallucinationFixtureRoot() *cobra.Command {
	root := &cobra.Command{Use: "pg_hardstorage"}
	dep := &cobra.Command{Use: "deployment"}
	add := &cobra.Command{Use: "add <name>", Short: "Add a new deployment"}
	add.Flags().String("connection", "", "libpq connection string")
	add.Flags().String("repo", "", "repository URL")
	dep.AddCommand(add)
	dep.AddCommand(&cobra.Command{Use: "list"})
	root.AddCommand(dep)
	return root
}

// TestSession_HallucinationCaughtBySuggestCommand: the
// model emits the operator's bug command via the
// suggest_command tool; the validating instance rejects
// it with a structured error; the orchestrator surfaces
// the rejection back to the model as the tool result so
// the model can retry on the next turn.  This is the
// load-bearing integration test for the end-to-end
// hallucination-resistance path.
func TestSession_HallucinationCaughtBySuggestCommand(t *testing.T) {
	tree := cmdtree.Walk(hallucinationFixtureRoot())
	reg := tools.NewRegistry()
	reg.Register(tools.SuggestCommand{Tree: tree})

	provider := &scriptedProvider{turns: []scriptedTurn{
		// Turn 1: model "answers" by suggesting the
		// hallucinated command.  This is what a real
		// model does when asked "how do I add a
		// deployment?" without ground truth.
		{toolCall: &llmprovider.ToolCallChunk{
			ID:   "tool_1",
			Name: "suggest_command",
			Args: map[string]any{
				"command": "pg_hardstorage deployment create --name mydb --connection postgres://x --repo /tmp/r",
				"why":     "to add a deployment",
			},
		}},
		// Turn 2: with the rejection in scope, the model
		// would emit a corrected suggestion in production.
		// In the test we just want to inspect what the
		// orchestrator handed it on the previous turn,
		// so we close out with a text turn.
		{text: "Sorry, I'll retry with the right verb.", usage: llmprovider.Usage{TotalTokens: 10}},
	}}

	s := &Session{Provider: provider, Tools: reg}
	reply, err := s.Ask(context.Background(), "how do I add a deployment?")
	if err != nil {
		t.Fatal(err)
	}
	if len(reply.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(reply.ToolCalls))
	}
	tc := reply.ToolCalls[0]
	if tc.Name != "suggest_command" {
		t.Fatalf("tool name = %q", tc.Name)
	}
	// The validator's rejection is the load-bearing fact.
	// In the result Body, error == "unknown_command" and
	// the message names the bad verb.
	bodyMap, ok := tc.Result.Body.(map[string]any)
	if !ok {
		t.Fatalf("expected map body, got %T: %+v", tc.Result.Body, tc.Result)
	}
	if got := bodyMap["error"]; got != "unknown_command" {
		t.Errorf("error = %v, want unknown_command", got)
	}
	if !strings.Contains(tc.Result.Summary, "rejected") {
		t.Errorf("summary should announce rejection; got %q", tc.Result.Summary)
	}

	// The 4th history message is the tool result echo
	// the orchestrator hands back to the model.  It
	// MUST contain the rejection details so the model
	// can react on the next turn.  Without this we'd
	// have a tree with eyes — the validator could see
	// the bug but the model couldn't see the validator.
	if len(s.History) < 4 {
		t.Fatalf("history length = %d, want >= 4", len(s.History))
	}
	tr := s.History[3]
	if tr.Role != "user" || tr.ToolUseID != "tool_1" {
		t.Fatalf("history[3] should be tool_result echo; got %+v", tr)
	}
	for _, want := range []string{"unknown_command", "create", "rejected"} {
		if !strings.Contains(tr.ToolResult, want) {
			t.Errorf("tool result echo to model missing %q\nfull result: %s", want, tr.ToolResult)
		}
	}
}

// TestSession_ValidSuggestCommandPassesThrough: the
// happy-path counterpart — when the model emits a real
// command, the validator passes it through and the
// suggestion reaches the operator unchanged.
func TestSession_ValidSuggestCommandPassesThrough(t *testing.T) {
	tree := cmdtree.Walk(hallucinationFixtureRoot())
	reg := tools.NewRegistry()
	reg.Register(tools.SuggestCommand{Tree: tree})

	provider := &scriptedProvider{turns: []scriptedTurn{
		{toolCall: &llmprovider.ToolCallChunk{
			ID:   "tool_1",
			Name: "suggest_command",
			Args: map[string]any{
				"command": "pg_hardstorage deployment add mydb --connection postgres://x --repo /tmp/r",
				"why":     "operator wants to add a deployment",
			},
		}},
		{text: "Run the command above.", usage: llmprovider.Usage{TotalTokens: 5}},
	}}
	s := &Session{Provider: provider, Tools: reg}
	reply, err := s.Ask(context.Background(), "how do I add a deployment?")
	if err != nil {
		t.Fatal(err)
	}
	tc := reply.ToolCalls[0]
	if !strings.HasPrefix(tc.Result.Summary, "suggested:") {
		t.Errorf("happy-path summary should start with \"suggested:\"; got %q", tc.Result.Summary)
	}
	bodyMap, _ := tc.Result.Body.(map[string]string)
	if bodyMap["command"] == "" {
		t.Errorf("happy-path body should preserve the command: %+v", tc.Result.Body)
	}
}

// TestSession_CommandCatalogReachesProvider: the catalog
// landed in the system prompt actually flows down to
// the provider's first call.  Without this the model
// never sees the ground-truth verb tree and we're back
// where we started.
func TestSession_CommandCatalogReachesProvider(t *testing.T) {
	tree := cmdtree.Walk(hallucinationFixtureRoot())
	provider := &scriptedProvider{turns: []scriptedTurn{
		{text: "ack", usage: llmprovider.Usage{TotalTokens: 1}},
	}}
	s := &Session{
		Provider:       provider,
		Tools:          tools.NewRegistry(),
		CommandCatalog: cmdtree.Catalog(tree, 2),
	}
	if _, err := s.Ask(context.Background(), "anything"); err != nil {
		t.Fatal(err)
	}
	if len(provider.captured) == 0 {
		t.Fatal("provider received no calls")
	}
	sysMsg := provider.captured[0][0]
	if sysMsg.Role != "system" {
		t.Fatalf("first message should be system; got %q", sysMsg.Role)
	}
	for _, want := range []string{
		"## Command catalog",
		"deployment",
		"add",
		"create-style verbs are spelled `add`",
	} {
		if !strings.Contains(sysMsg.Content, want) {
			t.Errorf("provider's system prompt missing %q", want)
		}
	}
}
