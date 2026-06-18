package cli_test

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// TestPartialDump_RequiresRepo: cobra-level missing-flag.
func TestPartialDump_RequiresRepo(t *testing.T) {
	_, stderr, exit := runCLI(t,
		"partial", "dump", "db1",
		"--tables", "public.users",
		"-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("missing --repo should exit Misuse; got %d\n%s", exit, stderr)
	}
}

// TestPartialDump_RequiresTables: --tables is mandatory.
func TestPartialDump_RequiresTables(t *testing.T) {
	w := newReadWorld(t)
	_, stderr, exit := runCLI(t,
		"partial", "dump", "db1",
		"--repo", w.repoURL,
		"-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("missing --tables should exit Misuse; got %d\n%s", exit, stderr)
	}
}

// TestPartialDump_PreflightFailsWithoutPGTools: when pg_ctl /
// pg_dump aren't on PATH, the pre-flight surfaces a structured
// error BEFORE doing the (potentially long) restore.
//
// We can't reliably make pg_ctl absent in CI (it might be there);
// to test this honestly we set PATH=/dev/null for the subprocess
// — but runCLI invokes in-process, so PATH manipulation has to
// happen via t.Setenv. We restore PATH automatically via t's
// cleanup.
func TestPartialDump_PreflightFailsWithoutPGTools(t *testing.T) {
	w := newReadWorld(t)
	// Empty PATH: exec.LookPath fails for everything.
	t.Setenv("PATH", "")
	_, stderr, exit := runCLI(t,
		"partial", "dump", "db1",
		"--repo", w.repoURL,
		"--tables", "public.users",
		"-o", "json")
	if exit == int(output.ExitOK) {
		t.Errorf("missing PG tools should fail; stderr=%s", stderr)
	}
	if !strings.Contains(stderr, "preflight.pg_tools_missing") {
		t.Errorf("expected preflight.pg_tools_missing code; got\n%s", stderr)
	}
	// The structured suggestion mentions postgresql-client.
	if !strings.Contains(stderr, "postgresql-client") {
		t.Errorf("error suggestion should mention postgresql-client:\n%s", stderr)
	}
}

// TestPartialDump_HelpListsCommand: cobra wires the dump
// subcommand visibly under `partial`.
func TestPartialDump_HelpListsCommand(t *testing.T) {
	stdout, _, _ := runCLI(t, "partial", "--help")
	if !strings.Contains(stdout, "dump") {
		t.Errorf("partial --help should list 'dump':\n%s", stdout)
	}
}

// TestPartialDump_HelpMentionsSkipVersionCheck: regression guard
// that the new --skip-version-check escape hatch shows up in
// help. Without it, an operator hitting a heterogeneous-fleet
// version mismatch can't find the bypass.
func TestPartialDump_HelpMentionsSkipVersionCheck(t *testing.T) {
	stdout, _, _ := runCLI(t, "partial", "dump", "--help")
	if !strings.Contains(stdout, "--skip-version-check") {
		t.Errorf("partial dump --help should advertise --skip-version-check:\n%s", stdout)
	}
}
