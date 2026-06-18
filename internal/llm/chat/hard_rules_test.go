package chat

import (
	"strings"
	"testing"
)

// TestHardRules_CommandCorrectness pins the always-injected guidance added
// for the LLM fix plan (#2 required flags, #3 destructive/dry-run semantics,
// #4 no path invention, #7 defer to tool remediation). The model can drift,
// but the prompt rules must not silently disappear.
func TestHardRules_CommandCorrectness(t *testing.T) {
	for _, want := range []string{
		// #2 — required flags
		"required flag not supplied",
		"--pg-connection <conn>",
		// #3 — destructive flags are not dry-runs
		"--apply, --force and --yes EXECUTE",
		"NOT dry-runs",
		"without touching anything",
		// #4 — no path/filename invention
		"manifest_signing.ed25519",
		"never a guessed name",
		`"unrecoverable"`,
		// #7 — defer to the tool's own remediation
		"suggestion.command",
		"read_command_help first",
		// round 2 — no hallucinated subcommands, check state, lead with one action
		"no \"backup full\"",
		"Check cluster STATE",
		"highest-likelihood next action",
		// round 4 — config GUCs that embed a command must include --repo
		"archive_command",
		"restore_command",
	} {
		if !strings.Contains(hardRulesAddendum, want) {
			t.Errorf("hardRulesAddendum is missing the directive %q", want)
		}
	}
}

// TestBuildSystemPrompt_InjectsHardRules: the rules actually reach the
// assembled prompt (a no-skill session still gets them).
func TestBuildSystemPrompt_InjectsHardRules(t *testing.T) {
	s := &Session{}
	prompt, err := s.buildSystemPrompt(nil)
	if err != nil {
		t.Fatalf("buildSystemPrompt: %v", err)
	}
	if !strings.Contains(prompt, "## Command correctness") {
		t.Errorf("assembled system prompt is missing the Command correctness rules")
	}
}

// TestHardRules_ConfigScheduleSchema pins the round-18 rule: the model must
// not invent flat backup_schedule/rotate_schedule YAML keys — the real schema
// nests cadence under schedule.{backup,rotate}.{every,daily_at,at}.
func TestHardRules_ConfigScheduleSchema(t *testing.T) {
	for _, want := range []string{
		"schedule schema is NESTED",
		"backup_schedule", // mentioned as the WRONG/invented key to avoid
		"schedule:",
		"--task=rotate",
		"don't invent config keys",
	} {
		if !strings.Contains(hardRulesAddendum, want) {
			t.Errorf("hardRulesAddendum missing config-schema directive %q", want)
		}
	}
}
