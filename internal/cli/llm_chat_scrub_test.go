package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/cli/cmdtree"
)

// scrubChatRoot is a small cobra fixture for scrub-render
// tests.  Mirror of the validator/scrub tests so we get
// the same operator-bug shape end-to-end.
func scrubChatRoot() *cobra.Command {
	root := &cobra.Command{Use: "pg_hardstorage"}
	dep := &cobra.Command{Use: "deployment"}
	add := &cobra.Command{Use: "add <name>"}
	add.Flags().String("connection", "", "")
	add.Flags().String("repo", "", "")
	dep.AddCommand(add)
	dep.AddCommand(&cobra.Command{Use: "list"})
	root.AddCommand(dep)
	return root
}

// TestRenderScrubFindings_OnlyEmitsForBad: when every
// finding parses cleanly we stay silent — the operator
// shouldn't see a "0 of 3 commands had problems" line.
func TestRenderScrubFindings_OnlyEmitsForBad(t *testing.T) {
	tree := cmdtree.Walk(scrubChatRoot())
	text := "Run `pg_hardstorage deployment add mydb --connection postgres://x --repo /tmp/r` to add it."
	findings := cmdtree.Scrub(tree, text, "pg_hardstorage")
	var buf bytes.Buffer
	renderScrubFindings(&buf, findings)
	if buf.Len() != 0 {
		t.Errorf("clean findings should produce no output; got %q", buf.String())
	}
}

// TestRenderScrubFindings_BadCommandShown: the operator's
// reported case rendered end-to-end through the chat
// streamer — assistant text contains the bug, the warn
// block names the bad command + the cobra error.
func TestRenderScrubFindings_BadCommandShown(t *testing.T) {
	tree := cmdtree.Walk(scrubChatRoot())
	text := "Try `pg_hardstorage deployment create --name mydb --connection postgres://x --repo /tmp/r`."
	findings := cmdtree.Scrub(tree, text, "pg_hardstorage")
	var buf bytes.Buffer
	renderScrubFindings(&buf, findings)
	out := buf.String()
	for _, want := range []string{
		"command-validation warnings",
		"deployment create",
		"unknown_command",
		"--help", // hint pointer
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered output should contain %q\nfull output:\n%s", want, out)
		}
	}
}

// TestRenderScrubFindings_MixedFindings: when prose
// contains both a good and a bad command, the renderer
// only warns about the bad one and the count line is
// "1 of 2".
func TestRenderScrubFindings_MixedFindings(t *testing.T) {
	tree := cmdtree.Walk(scrubChatRoot())
	text := "First `pg_hardstorage deployment list`, then `pg_hardstorage deployment create --name x`."
	findings := cmdtree.Scrub(tree, text, "pg_hardstorage")
	var buf bytes.Buffer
	renderScrubFindings(&buf, findings)
	out := buf.String()
	if !strings.Contains(out, "1 of 2") {
		t.Errorf("mixed findings should report \"1 of 2\"; got %q", out)
	}
	if strings.Contains(out, "deployment list") {
		t.Errorf("renderer should not echo the valid command in the warning block: %q", out)
	}
}
