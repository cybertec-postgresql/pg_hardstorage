package chat

import (
	"strings"
	"testing"
)

// TestExtractAgentCommands_IgnoresProseAndComments is the regression for
// the noisy command-validator: a prose sentence that begins with
// "pg_hardstorage" and a "# ..." comment inside a fence must NOT be
// extracted as commands (they produced spurious "unknown subcommand
// 'does'/'handles'" warnings that trained operators to ignore the
// validator). Only fenced command lines and inline backtick spans count.
func TestExtractAgentCommands_IgnoresProseAndComments(t *testing.T) {
	text := strings.Join([]string{
		"pg_hardstorage does not support table-level scheduled backups.", // prose
		"",
		"```bash",
		"# pg_hardstorage handles the full backup — already configured", // comment
		"pg_hardstorage backup db1 --repo file:///r",                    // real command
		"```",
		"",
		"Also run `pg_hardstorage status` weekly.", // inline → Pass 2
	}, "\n")

	cmds := extractAgentCommands(text)

	joined := strings.Join(cmds, " || ")
	if strings.Contains(joined, "does not support") || strings.Contains(joined, "handles the full") {
		t.Errorf("prose/comment leaked into extracted commands: %#v", cmds)
	}
	if !strings.Contains(joined, "backup db1 --repo") {
		t.Errorf("fenced command not extracted: %#v", cmds)
	}
	if !contains(cmds, "pg_hardstorage status") {
		t.Errorf("inline backtick command not extracted: %#v", cmds)
	}
	if len(cmds) != 2 {
		t.Errorf("expected exactly 2 commands (fenced backup + inline status), got %d: %#v", len(cmds), cmds)
	}
}

// TestExtractAgentCommands_UnfencedProseLineIgnored: a bare line that
// starts with the binary name but is NOT inside a fence is prose, not a
// command.
func TestExtractAgentCommands_UnfencedProseLineIgnored(t *testing.T) {
	text := "pg_hardstorage will refuse to start as root.\n\nThat's the whole answer."
	if cmds := extractAgentCommands(text); len(cmds) != 0 {
		t.Errorf("unfenced prose should yield no commands, got: %#v", cmds)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// TestExtractAgentCommands_ConfigEmbedded is the round-4 #2 regression:
// pg_hardstorage commands embedded in quoted config values inside a fenced
// block (archive_command / restore_command) must be extracted so the
// validator can flag a missing --repo.
func TestExtractAgentCommands_ConfigEmbedded(t *testing.T) {
	text := strings.Join([]string{
		"Set these in postgresql.conf:",
		"```",
		"archive_command = 'pg_hardstorage wal push db1 %p'",
		"restore_command = \"pg_hardstorage wal fetch db1 %f %p --repo file:///r\"",
		"```",
	}, "\n")
	cmds := extractAgentCommands(text)
	joined := strings.Join(cmds, " || ")
	if !strings.Contains(joined, "pg_hardstorage wal push db1 %p") {
		t.Errorf("archive_command not extracted from the quoted config value: %#v", cmds)
	}
	if !strings.Contains(joined, "pg_hardstorage wal fetch db1 %f %p --repo file:///r") {
		t.Errorf("restore_command not extracted: %#v", cmds)
	}
}

// TestExtractAgentCommands_QuotedProseOutsideFenceIgnored: a quoted command
// in PROSE (outside any fence) is not pulled in by Pass 3 — only fenced
// config snippets count, keeping false positives out.
func TestExtractAgentCommands_QuotedProseOutsideFenceIgnored(t *testing.T) {
	text := "Don't run 'pg_hardstorage repo wipe --yes' lightly — it destroys everything."
	for _, c := range extractAgentCommands(text) {
		if strings.Contains(c, "repo wipe") {
			t.Errorf("Pass 3 must not extract quoted prose outside a fence: %q", c)
		}
	}
}
