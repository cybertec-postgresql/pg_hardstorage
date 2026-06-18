package cli_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/cli"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/cli/cmdtree"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/chat"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/skills"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/tools"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/llmprovider"
)

// TestLLMEval_RealProviderHallucinationResistance is a
// nightly-canary eval that exercises the
// hallucination-resistance stack against a real model.
// It is opt-in: gated on PG_HARDSTORAGE_LLM_EVAL=1 AND
// the standard provider env vars (PG_HARDSTORAGE_LLM_KEY
// or OPENAI_API_KEY).  Default CI skips it — the suite
// is meant for the nightly job that watches for model
// drift, and for the developer who wants to verify a
// prompt change end-to-end before shipping.
//
// The test asserts a single high-bar property: when the
// operator asks "how do I X?", the model's suggested
// command (whether emitted via suggest_command or
// inlined in prose) parses against the live cobra tree.
// This is the property that was broken when the
// operator hit `deployment create --name X` — the
// validator + system-prompt catalog should make that
// shape unreachable from a real model with the right
// scaffolding.
//
// Failure modes flagged:
//   - Test skipped: usual case, no eval env set.
//   - Test fails: model suggested a command that did NOT
//     parse — investigate prompt regression.
//   - Test errors: provider config problem (key, model,
//     network) — fix the env, not the prompt.
func TestLLMEval_RealProviderHallucinationResistance(t *testing.T) {
	if os.Getenv("PG_HARDSTORAGE_LLM_EVAL") != "1" {
		t.Skip("set PG_HARDSTORAGE_LLM_EVAL=1 to run real-provider evals (nightly canary, not default CI)")
	}
	apiKey := os.Getenv("PG_HARDSTORAGE_LLM_KEY")
	if apiKey == "" {
		apiKey = os.Getenv("OPENAI_API_KEY")
	}
	if apiKey == "" {
		t.Skip("PG_HARDSTORAGE_LLM_KEY (or OPENAI_API_KEY) not set; cannot run real-provider eval")
	}

	// Resolve the provider via the same precedence the CLI
	// uses so the eval matches production resolution.
	prov, err := llmprovider.DefaultRegistry.Get("openai")
	if err != nil {
		t.Fatalf("openai provider unavailable: %v", err)
	}
	cfg := llmprovider.ProviderConfig{
		APIKey:   apiKey,
		Endpoint: firstNonEmptyEnv("PG_HARDSTORAGE_URL", "OPENAI_BASE_URL"),
		Model:    firstNonEmptyEnv("PG_HARDSTORAGE_LLM_MODEL", "OPENAI_MODEL"),
	}
	if err := prov.Open(context.Background(), cfg); err != nil {
		t.Fatalf("provider open: %v", err)
	}
	defer prov.Close()

	// Build the introspector tree from the live cobra
	// root — this is what the validator + scrubber will
	// check against, AND what the system-prompt catalog
	// is rendered from.  Both ends of the eval use the
	// same source of truth.
	root := cli.NewRoot()
	tree := cmdtree.Walk(root)

	// Wire a session that mirrors the production chat
	// session: validating SuggestCommand, ReadCommandHelp
	// available, ask-skill prompt template loaded from
	// the embedded skill set.
	skillSet, err := skills.LoadBuiltins()
	if err != nil {
		t.Fatalf("skill load: %v", err)
	}
	skill, err := skillSet.Get("ask")
	if err != nil {
		t.Fatalf("ask skill: %v", err)
	}
	reg := tools.NewRegistry()
	reg.Register(tools.SuggestCommand{Tree: tree})
	reg.Register(tools.ReadCommandHelp{Tree: tree})

	// The eval prompts: questions whose right answer is a
	// real `pg_hardstorage` command.  Each prompt is a
	// natural way an operator might phrase the question.
	// We don't dictate which command the model should
	// pick — the property under test is "whatever you
	// suggest, it must parse against the live tree."
	prompts := []struct {
		name   string
		prompt string
	}{
		{"add_deployment",
			"How do I add a new deployment named mydb1 with connection string 'postgres://hs:pass@localhost:5432/postgres' and repo at /tmp/repo?"},
		{"take_backup",
			"Operator: I want to take an immediate backup of deployment mydb1.  What's the command?"},
		{"list_deployments",
			"Show me how to list all configured deployments."},
		{"check_repo",
			"How do I verify the integrity of a repository at /tmp/repo?"},
	}

	for _, tc := range prompts {
		t.Run(tc.name, func(t *testing.T) {
			session := &chat.Session{
				Provider:       prov,
				Tools:          reg,
				Skill:          skill,
				CommandCatalog: cmdtree.Catalog(tree, 2),
			}
			reply, err := session.Ask(context.Background(), tc.prompt)
			if err != nil {
				t.Fatalf("Ask: %v", err)
			}
			// Collect every command surface the model
			// produced — both the suggest_command tool
			// args AND any backtick-wrapped commands in
			// the prose answer — and validate each.
			var commands []string
			for _, tcCall := range reply.ToolCalls {
				if tcCall.Name != "suggest_command" {
					continue
				}
				if cmdStr, _ := tcCall.Args["command"].(string); cmdStr != "" {
					commands = append(commands, cmdStr)
				}
			}
			for _, finding := range cmdtree.Scrub(tree, reply.Text, "pg_hardstorage") {
				commands = append(commands, finding.Command)
			}
			if len(commands) == 0 {
				t.Logf("model produced no command surface for %q — assistant text:\n%s",
					tc.prompt, reply.Text)
				// Not a hard failure — the model may
				// legitimately answer "you need to set
				// up a deployment first" for some
				// prompts.  The eval property is "if
				// the model produces a command, it must
				// be valid", not "the model must always
				// produce a command".
				return
			}
			for _, c := range commands {
				if err := cmdtree.Validate(tree, c, "pg_hardstorage"); err != nil {
					t.Errorf("model produced command that does not parse:\n  %s\n  validator says: %v",
						c, err)
				}
			}
		})
	}
}

func firstNonEmptyEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// stop unused-import warning when the test is skipped.
var _ = strings.Contains
