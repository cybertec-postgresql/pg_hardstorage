package tools_test

import (
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/cli/cmdtree"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/tools"
)

// fixtureRoot mirrors the operator's reported bug: a
// `deployment add <name>` command that takes --connection
// + --repo, so the tool's lookup output proves it would
// have steered the model away from "deployment create
// --name".
func fixtureRoot() *cobra.Command {
	root := &cobra.Command{Use: "pg_hardstorage"}
	dep := &cobra.Command{Use: "deployment", Short: "Manage deployments"}
	add := &cobra.Command{Use: "add <name>", Short: "Add a new deployment"}
	add.Flags().String("connection", "", "libpq connection string (required)")
	add.Flags().String("repo", "", "repository URL (required)")
	add.Flags().Bool("skip-probe", false, "don't probe PG")
	dep.AddCommand(add)
	dep.AddCommand(&cobra.Command{Use: "list", Short: "List deployments"})
	root.AddCommand(dep)
	return root
}

func TestReadCommandHelp_DeploymentAdd(t *testing.T) {
	tree := cmdtree.Walk(fixtureRoot())
	tool := tools.ReadCommandHelp{Tree: tree}

	res, err := tool.Run(context.Background(), map[string]any{"command": "deployment add"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	body, ok := res.Body.(map[string]any)
	if !ok {
		t.Fatalf("Body type = %T, want map[string]any", res.Body)
	}
	help, _ := body["help"].(string)
	for _, want := range []string{"deployment add", "Add a new deployment", "--connection", "--repo", "--skip-probe"} {
		if !strings.Contains(help, want) {
			t.Errorf("help missing %q\nfull:\n%s", want, help)
		}
	}
}

func TestReadCommandHelp_StripsBinaryPrefix(t *testing.T) {
	tree := cmdtree.Walk(fixtureRoot())
	tool := tools.ReadCommandHelp{Tree: tree}
	// Forgivable mistake: model passes the binary name in
	// the path.  Tool should strip it.
	res, err := tool.Run(context.Background(), map[string]any{"command": "pg_hardstorage deployment add"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	body, _ := res.Body.(map[string]any)
	help, _ := body["help"].(string)
	if !strings.Contains(help, "deployment add") {
		t.Errorf("binary-prefixed lookup should resolve; got body %v", body)
	}
}

func TestReadCommandHelp_UnknownCommandReturnsAvailable(t *testing.T) {
	tree := cmdtree.Walk(fixtureRoot())
	tool := tools.ReadCommandHelp{Tree: tree}

	// The actual bug — model asks for "deployment create".
	// The tool must steer the model toward the real verbs.
	res, err := tool.Run(context.Background(), map[string]any{"command": "deployment create"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	body, _ := res.Body.(map[string]any)
	if body["error"] != "command_not_found" {
		t.Errorf("error code = %v, want command_not_found", body["error"])
	}
	if body["resolved_prefix"] != "deployment" {
		t.Errorf("resolved_prefix = %v, want \"deployment\"", body["resolved_prefix"])
	}
	avail, _ := body["available_at_prefix"].([]string)
	hasAdd := false
	for _, v := range avail {
		if v == "add" {
			hasAdd = true
		}
	}
	if !hasAdd {
		t.Errorf("available_at_prefix should include \"add\"; got %v", avail)
	}
}

func TestReadCommandHelp_EmptyCommandIsError(t *testing.T) {
	tree := cmdtree.Walk(fixtureRoot())
	tool := tools.ReadCommandHelp{Tree: tree}
	if _, err := tool.Run(context.Background(), map[string]any{"command": ""}); err == nil {
		t.Error("empty command should error")
	}
}

func TestReadCommandHelp_NoTreeDegradesGracefully(t *testing.T) {
	// When the registry is built without a cobra root
	// (test paths, MCP fallback), the tool should not
	// panic — it returns a structured "tool unavailable"
	// Result so the model knows to fall back.
	tool := tools.ReadCommandHelp{Tree: nil}
	res, err := tool.Run(context.Background(), map[string]any{"command": "deployment add"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	body, _ := res.Body.(map[string]any)
	if body["error"] != "tool_unavailable" {
		t.Errorf("error code = %v, want tool_unavailable", body["error"])
	}
}

func TestSuggestCommand_HappyPath(t *testing.T) {
	tree := cmdtree.Walk(fixtureRoot())
	tool := tools.SuggestCommand{Tree: tree}

	res, err := tool.Run(context.Background(), map[string]any{
		"command": "pg_hardstorage deployment add mydb --connection postgres://x --repo /tmp/r",
		"why":     "operator wants to add a deployment",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.HasPrefix(res.Summary, "suggested:") {
		t.Errorf("happy-path summary should start with \"suggested:\"; got %q", res.Summary)
	}
}

// TestSuggestCommand_TheActualBug: the validator catches
// the operator's reported case.  This is the load-bearing
// regression test — if it ever fails, an LLM session can
// dump fictional commands at the operator again.
func TestSuggestCommand_TheActualBug(t *testing.T) {
	tree := cmdtree.Walk(fixtureRoot())
	tool := tools.SuggestCommand{Tree: tree}

	res, err := tool.Run(context.Background(), map[string]any{
		"command": "pg_hardstorage deployment create --name mydb --connection postgres://x --repo /tmp/r",
		"why":     "operator wants to add a deployment",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	body, ok := res.Body.(map[string]any)
	if !ok {
		t.Fatalf("Body type = %T, want map[string]any (validator should produce a structured rejection, not echo)", res.Body)
	}
	if body["error"] != "unknown_command" {
		t.Errorf("error code = %v, want unknown_command", body["error"])
	}
	if !strings.Contains(res.Summary, "rejected") {
		t.Errorf("summary should mark the suggestion as rejected; got %q", res.Summary)
	}
	if rej, _ := body["rejected_command"].(string); !strings.Contains(rej, "create") {
		t.Errorf("body should preserve the rejected command; got %v", body)
	}
}

func TestSuggestCommand_UnknownFlag(t *testing.T) {
	tree := cmdtree.Walk(fixtureRoot())
	tool := tools.SuggestCommand{Tree: tree}

	res, err := tool.Run(context.Background(), map[string]any{
		"command": "pg_hardstorage deployment add mydb --name foo --connection postgres://x",
		"why":     "operator wants to add a deployment",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	body, _ := res.Body.(map[string]any)
	if body["error"] != "unknown_flag" {
		t.Errorf("error code = %v, want unknown_flag", body["error"])
	}
	if !strings.Contains(body["message"].(string), "--name") {
		t.Errorf("message should name the bad flag; got %v", body["message"])
	}
}

func TestSuggestCommand_NoTreeFallsBackToEcho(t *testing.T) {
	// Tree=nil preserves the pre-Layer-3 echo-only contract
	// so test paths and MCP fallbacks keep working.
	tool := tools.SuggestCommand{Tree: nil}
	res, err := tool.Run(context.Background(), map[string]any{
		"command": "pg_hardstorage deployment create --name x", // would be rejected with a tree
		"why":     "test",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.HasPrefix(res.Summary, "suggested:") {
		t.Errorf("nil-tree path should echo-only; got %q", res.Summary)
	}
}

func TestSuggestCommand_MissingCommandIsError(t *testing.T) {
	tool := tools.SuggestCommand{Tree: cmdtree.Walk(fixtureRoot())}
	if _, err := tool.Run(context.Background(), map[string]any{"command": "", "why": "x"}); err == nil {
		t.Error("empty command should error")
	}
}
