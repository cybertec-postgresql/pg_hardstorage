package cli_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/cli"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// runChat drives the chat REPL with scripted stdin lines.
// Returns stdout + exit code.
func runChat(t *testing.T, stdin string, args ...string) (stdout string, exit int) {
	t.Helper()
	root := cli.NewRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(append([]string{"llm", "chat"}, args...))
	exit = cli.Run(root)
	return out.String(), exit
}

// TestLlmChat_Banner shows the welcome banner including
// skill/provider/model/url info and the verify-suggestion
// disclaimer.  ANSI escapes must not appear here — stdout
// is a bytes.Buffer (not a TTY), so newBannerStyle should
// degrade to plain text.
func TestLlmChat_Banner(t *testing.T) {
	// EOF on stdin => clean exit before any prompt.
	stdout, exit := runChat(t, "", "--provider", "mock", "--endpoint", "http://localhost:11434/v1", "--model", "llama3.1:8b")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d; out=%s", exit, stdout)
	}
	if !strings.Contains(stdout, "AI assistant") {
		t.Errorf("banner missing AI-assistant disclaimer: %q", stdout)
	}
	if !strings.Contains(stdout, "skill: ask") {
		t.Errorf("banner should name the active skill; got %q", stdout)
	}
	if !strings.Contains(stdout, "provider: mock") {
		t.Errorf("banner should name the provider; got %q", stdout)
	}
	if !strings.Contains(stdout, "model: llama3.1:8b") {
		t.Errorf("banner should show the resolved model; got %q", stdout)
	}
	if !strings.Contains(stdout, "url: http://localhost:11434/v1") {
		t.Errorf("banner should show the resolved endpoint URL; got %q", stdout)
	}
	if strings.Contains(stdout, "\x1b[") {
		t.Errorf("banner emitted ANSI escapes to a non-TTY writer: %q", stdout)
	}
}

// TestLlmChat_OneTurnEcho: a single user line goes through the
// mock provider, which echoes it back with a "mock-reply:"
// prefix; the chat loop prints the reply + a token-cost line.
func TestLlmChat_OneTurnEcho(t *testing.T) {
	stdout, exit := runChat(t, "what's up?\n", "--provider", "mock")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d; out=%s", exit, stdout)
	}
	if !strings.Contains(stdout, "mock-reply: what's up?") {
		t.Errorf("expected mock-reply echo; got %q", stdout)
	}
	if !strings.Contains(stdout, "tokens") {
		t.Errorf("token-cost line missing; got %q", stdout)
	}
}

// TestLlmChat_SlashHelp: /help prints the slash-command list.
func TestLlmChat_SlashHelp(t *testing.T) {
	stdout, _ := runChat(t, "/help\n", "--provider", "mock")
	for _, want := range []string{"/exit", "/clear", "/show-context", "/show-tools", "/show-skill", "/show-budget"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("/help should list %q; got %q", want, stdout)
		}
	}
}

// TestLlmChat_SlashExit: /exit terminates the loop cleanly.
func TestLlmChat_SlashExit(t *testing.T) {
	stdout, exit := runChat(t, "/exit\n", "--provider", "mock")
	if exit != int(output.ExitOK) {
		t.Errorf("/exit should exit cleanly; got %d, out=%s", exit, stdout)
	}
}

func TestLlmChat_SlashClear(t *testing.T) {
	// First send a message (grows history), then /clear, then
	// /show-context (history length back to 1 = system prompt).
	stdin := "hello\n/clear\n/show-context\n"
	stdout, _ := runChat(t, stdin, "--provider", "mock")
	if !strings.Contains(stdout, "conversation cleared") {
		t.Errorf("/clear should announce cleared; got %q", stdout)
	}
	// After /clear, the show-context messages count should be 1.
	idx := strings.Index(stdout, "conversation cleared")
	tail := stdout[idx:]
	// We expect the snapshot JSON to include "messages": 1 (the
	// system prompt).
	if !strings.Contains(tail, "\"messages\": 1") {
		t.Errorf("post-/clear snapshot should show 1 message; got %q", tail)
	}
}

func TestLlmChat_SlashShowSkill(t *testing.T) {
	stdout, _ := runChat(t, "/show-skill\n", "--provider", "mock")
	for _, want := range []string{"name:", "version:", "source:"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("/show-skill should print %q; got %q", want, stdout)
		}
	}
}

func TestLlmChat_SlashShowTools(t *testing.T) {
	stdout, _ := runChat(t, "/show-tools\n", "--provider", "mock")
	// Live-state tools (read_doctor, read_status, ...) are
	// skipped in the test binary because forking go-test with
	// CLI args would hang.  The always-safe set should still
	// be visible: preview_command, read_runbook,
	// suggest_command.
	for _, want := range []string{"preview_command", "read_runbook", "suggest_command"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("/show-tools should list %q for the ask skill (always-safe set); got %q", want, stdout)
		}
	}
}

func TestLlmChat_UnknownSlashRejected(t *testing.T) {
	stdout, _ := runChat(t, "/nonsense\n", "--provider", "mock")
	if !strings.Contains(stdout, "unknown command") {
		t.Errorf("expected unknown-command error; got %q", stdout)
	}
}

func TestLlmChat_DefaultSubcommandIsChat(t *testing.T) {
	// `pg_hardstorage llm` (no subcommand) drops into chat.  We
	// don't go through runChat (which forces 'chat'); use the
	// helper that runCLI provides.
	root := cli.NewRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(strings.NewReader(""))
	root.SetArgs([]string{"llm", "--provider", "mock"})
	if exit := cli.Run(root); exit != int(output.ExitOK) {
		t.Fatalf("exit = %d; out=%s", exit, out.String())
	}
	if !strings.Contains(out.String(), "AI assistant") {
		t.Errorf("`llm` no-subcommand should drop into chat; got %q", out.String())
	}
}
