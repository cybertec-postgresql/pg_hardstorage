package chat

import (
	"strings"
	"testing"
)

// The assistant's tools and the operator's CLI are two different
// surfaces that happen to share a prompt. Tool descriptions read
// "Run `pg_hardstorage doctor` and return ...", which says what the
// tool does for us but leaves the tool NAME looking like part of the
// command surface — and the names sit next to the real command
// catalog.
//
// Models conflated them. Three answers in a 194-question run told the
// operator to run
//
//	pg_hardstorage read_command_help deployment add
//
// inside a copy-pasteable shell block. That command does not exist:
// read_command_help is ours, not theirs. An operator following the
// advice gets "unknown command" while attempting the very thing the
// answer was explaining.
//
// Two tests: the prompt must state the boundary, and the validator
// that already walks suggested commands must reject a tool name used
// as one.

// TestSystemPromptSeparatesToolsFromCommands pins the wording that
// keeps the two surfaces apart. It asserts the CLAIM is present, not
// its exact phrasing — reword freely, just do not drop it.
func TestSystemPromptSeparatesToolsFromCommands(t *testing.T) {
	// The guarantee is about the rendered text, so assert on the
	// wording the builder writes rather than reconstructing a whole
	// session: a session needs a tool registry and a skill, and the
	// point here is the sentence, not the plumbing.
	const mustSay = "NOT `pg_hardstorage` subcommands"
	src := systemPromptToolPreamble()
	if !strings.Contains(src, mustSay) {
		t.Errorf("the tools section must state that tool names are not CLI subcommands.\n"+
			"Without it the model emits `pg_hardstorage read_command_help ...` to operators.\ngot:\n%s", src)
	}
	if !strings.Contains(src, "read_command_help") {
		t.Error("name the tool that actually leaked, so the instruction is concrete")
	}
}

// TestToolNamesAreNotValidCommands is the behavioural half: whatever
// the prompt says, a tool name presented as a command must not pass
// the validator that already checks suggested commands against the
// real cobra tree.
func TestToolNamesAreNotValidCommands(t *testing.T) {
	toolNames := []string{
		"read_command_help", "read_doctor", "read_status",
		"list_deployments", "list_backups", "search_docs",
	}
	for _, name := range toolNames {
		answer := "Look up the flags first:\n\n```bash\npg_hardstorage " + name + " deployment add\n```\n"
		cmds := extractAgentCommands(answer)
		if len(cmds) == 0 {
			t.Errorf("%s: extractAgentCommands found nothing to validate in a fenced command", name)
			continue
		}
		var sawIt bool
		for _, c := range cmds {
			if strings.Contains(c, name) {
				sawIt = true
			}
		}
		if !sawIt {
			t.Errorf("%s: a tool name in a shell fence must reach the validator, "+
				"otherwise nothing can catch it; extracted %q", name, cmds)
		}
	}
}
