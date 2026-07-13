package cli

import (
	"bytes"
	"strings"
	"testing"
)

// Regression: `pg_hardstorage --version` must work (CLI muscle memory) and
// print the same one-liner as the `version` subcommand's text renderer.
// It previously failed with `unknown flag: --version` (exit 2).
func TestRootVersionFlag(t *testing.T) {
	root := NewRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--version"})
	if err := root.Execute(); err != nil {
		t.Fatalf("--version: %v", err)
	}
	got := strings.TrimSpace(out.String())
	if !strings.HasPrefix(got, "pg_hardstorage ") || !strings.Contains(got, "built") {
		t.Errorf("--version output = %q, want the `pg_hardstorage <ver> (<commit>, built <date>)` one-liner", got)
	}
}
