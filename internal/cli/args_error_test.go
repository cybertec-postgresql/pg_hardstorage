package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// fixtureRestoreCmd mirrors the real restore command's
// shape: ExactArgs(2) with two named placeholders.  Used
// by the enricher tests to exercise the full path
// without depending on the live cobra root (so the test
// keeps passing when restore evolves).
func fixtureRestoreCmd() *cobra.Command {
	root := &cobra.Command{Use: "pg_hardstorage"}
	restore := &cobra.Command{
		Use:  "restore <deployment> <backup-id|latest>",
		Args: cobra.ExactArgs(2),
		RunE: func(*cobra.Command, []string) error { return nil },
	}
	root.AddCommand(restore)
	return restore
}

func TestEnrichArgsError_TheActualBug(t *testing.T) {
	cmd := fixtureRestoreCmd()
	// Simulate the operator's case: 1 arg supplied, 2
	// required.  We construct the cobra error verbatim
	// because that's what Args validation produces.
	original := errors.New("accepts 2 arg(s), received 1")
	got := enrichArgsError(cmd, original)
	if got == nil {
		t.Fatal("enrichArgsError returned nil; expected an enriched error")
	}
	msg := got.Error()
	for _, want := range []string{
		"pg_hardstorage restore",
		"needs 2 arguments (got 1)",
		"expected: <deployment> <backup-id|latest>",
		"example:  pg_hardstorage restore mydb1 latest",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("enriched message missing %q\nfull:\n%s", want, msg)
		}
	}
}

func TestEnrichArgsError_ZeroArgs(t *testing.T) {
	cmd := fixtureRestoreCmd()
	got := enrichArgsError(cmd, errors.New("accepts 2 arg(s), received 0"))
	if !strings.Contains(got.Error(), "got 0") {
		t.Errorf("zero-args case should report (got 0): %q", got)
	}
}

func TestEnrichArgsError_OneRequired(t *testing.T) {
	root := &cobra.Command{Use: "pg_hardstorage"}
	add := &cobra.Command{
		Use:  "add <name>",
		Args: cobra.ExactArgs(1),
		RunE: func(*cobra.Command, []string) error { return nil },
	}
	dep := &cobra.Command{Use: "deployment"}
	dep.AddCommand(add)
	root.AddCommand(dep)

	got := enrichArgsError(add, errors.New("accepts 1 arg(s), received 0"))
	msg := got.Error()
	if !strings.Contains(msg, "needs 1 argument") {
		t.Errorf("singular argument count: %q", msg)
	}
	if !strings.Contains(msg, "expected: <name>") {
		t.Errorf("expected line: %q", msg)
	}
	if !strings.Contains(msg, "pg_hardstorage deployment add mydb1") {
		t.Errorf("example should substitute <name>: %q", msg)
	}
}

func TestEnrichArgsError_AtLeastN(t *testing.T) {
	cmd := fixtureRestoreCmd()
	got := enrichArgsError(cmd, errors.New("requires at least 1 arg(s), only received 0"))
	if !strings.Contains(got.Error(), "needs at least 1 argument") {
		t.Errorf("at-least-N rewriting: %q", got)
	}
}

func TestEnrichArgsError_AtMostN(t *testing.T) {
	cmd := fixtureRestoreCmd()
	got := enrichArgsError(cmd, errors.New("accepts at most 3 arg(s), received 5"))
	if !strings.Contains(got.Error(), "accepts at most 3 arguments (got 5)") {
		t.Errorf("at-most-N rewriting: %q", got)
	}
}

func TestEnrichArgsError_Range(t *testing.T) {
	cmd := fixtureRestoreCmd()
	got := enrichArgsError(cmd, errors.New("accepts between 1 and 3 arg(s), received 0"))
	if !strings.Contains(got.Error(), "needs 1–3 arguments") {
		t.Errorf("range rewriting: %q", got)
	}
}

// TestEnrichArgsError_NonArgErrorPassesThrough: only
// arg-count failures get enriched; everything else
// passes through unchanged so we don't accidentally
// wrap unrelated errors with usage-error semantics.
func TestEnrichArgsError_NonArgErrorPassesThrough(t *testing.T) {
	cmd := fixtureRestoreCmd()
	original := errors.New("connection refused: dial tcp 127.0.0.1:5432")
	got := enrichArgsError(cmd, original)
	if got != original {
		t.Errorf("non-args error should pass through unchanged; got %v", got)
	}
}

func TestEnrichArgsError_NilInputs(t *testing.T) {
	if got := enrichArgsError(nil, errors.New("anything")); got == nil {
		t.Error("nil cmd should return error unchanged, not nil")
	}
	if got := enrichArgsError(fixtureRestoreCmd(), nil); got != nil {
		t.Error("nil error should stay nil")
	}
}

func TestEnrichArgsError_ExplicitExampleHonoured(t *testing.T) {
	cmd := &cobra.Command{
		Use:     "wal stream <deployment>",
		Args:    cobra.ExactArgs(1),
		Example: "  pg_hardstorage wal stream mydb1 --pg-connection postgres://h",
	}
	root := &cobra.Command{Use: "pg_hardstorage"}
	root.AddCommand(cmd)

	got := enrichArgsError(cmd, errors.New("accepts 1 arg(s), received 0"))
	if !strings.Contains(got.Error(), "--pg-connection postgres://h") {
		t.Errorf("explicit Example should win over synthesised: %q", got)
	}
}

func TestPositionalPlaceholders(t *testing.T) {
	cases := map[string]string{
		"restore <deployment> <backup-id|latest>": "<deployment> <backup-id|latest>",
		"deployment": "",
		"add <name>": "<name>",
		"  list   ":  "",
	}
	for in, want := range cases {
		if got := positionalPlaceholders(in); got != want {
			t.Errorf("positionalPlaceholders(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestRun_ArgsErrorRendersFriendlyMessage drives the real
// cobra root via Run() with the operator's exact command
// line and asserts the rendered stderr is the friendly
// shape — not cobra's bare "accepts 2 arg(s), received 1".
// This is the regression test that locks the bug fix in
// against any future change that removes the enricher
// hook from Run().
func TestRun_ArgsErrorRendersFriendlyMessage(t *testing.T) {
	root := NewRoot()
	var stderr strings.Builder
	root.SetOut(&stderr)
	root.SetErr(&stderr)
	root.SetArgs([]string{"restore", "hans1"})
	exit := Run(root)
	if exit != 2 {
		t.Errorf("exit code = %d, want 2 (usage error)", exit)
	}
	out := stderr.String()
	for _, want := range []string{
		"pg_hardstorage restore",
		"needs 2 arguments (got 1)",
		"expected:",
		"example:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered output missing %q\nfull output:\n%s", want, out)
		}
	}
	// Specifically assert the structured-code prefix is
	// gone — that's the readability win.
	if strings.Contains(out, "usage.bad_args:") {
		t.Errorf("structured code prefix should not appear in text output: %q", out)
	}
}

func TestSubstituteExampleTokens(t *testing.T) {
	in := "<deployment> <backup-id|latest>"
	want := "mydb1 latest"
	if got := substituteExampleTokens(in); got != want {
		t.Errorf("substituteExampleTokens(%q) = %q, want %q", in, got, want)
	}
	// Unrecognised placeholder stays verbatim — better
	// than substituting a misleading default.
	if got := substituteExampleTokens("<thingamajig>"); got != "<thingamajig>" {
		t.Errorf("unknown placeholder should pass through: %q", got)
	}
}
