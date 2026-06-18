package cli_test

import (
	"strings"
	"testing"
)

// TestPartialDump_HasDatabaseFlag is the regression guard for issue #97:
// partial dump must expose a --database flag so a table living outside
// the default "postgres" database can be reached.
func TestPartialDump_HasDatabaseFlag(t *testing.T) {
	stdout, _, _ := runCLI(t, "partial", "dump", "--help")
	if !strings.Contains(stdout, "--database") {
		t.Errorf("partial dump --help should document --database:\n%s", stdout)
	}
	// The help should explain that a single database is connected to,
	// so the operator understands when they need the flag.
	if !strings.Contains(stdout, "single database") && !strings.Contains(stdout, "one database") {
		t.Errorf("--database help should explain the single-database constraint:\n%s", stdout)
	}
}
