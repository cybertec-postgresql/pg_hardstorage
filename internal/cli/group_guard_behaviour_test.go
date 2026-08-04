package cli_test

// group_guard_behaviour_test.go — what the group guard is FOR.
//
// dump_cmd_tree_group_guard_test.go checks that the annotation is
// present and applied to the right commands, because the CLI coverage
// gate skips anything carrying it. That says nothing about the
// behaviour the annotation exists to mark.
//
// hardenGroupCommands synthesises a RunE on every pure group so a
// mistyped subcommand FAILS. Without it cobra runs the group itself,
// which prints help and exits 0 — a typo in a cron entry or a runbook
// then looks like a successful run, and a backup nobody took reports
// success until someone checks the repo.
//
// So: a bare group prints help and exits 0; a group with a bogus
// argument exits non-zero as a usage error.

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// runGroup invokes one argv through the shared runCLI helper
// (listshowstatus_test.go) and folds its two streams together — help
// goes to stdout, the usage error to stderr, and both cases matter
// here.
func runGroup(t *testing.T, args ...string) (int, string) {
	t.Helper()
	stdout, stderr, exit := runCLI(t, args...)
	return exit, stdout + stderr
}

// groupsUnderTest are pure groups spread across the command tree,
// including a nested one — the guard recurses, and a nested group is
// where a missed recursion would hide.
var groupsUnderTest = [][]string{
	{"kms"},
	{"audit"},
	{"repo"},
	{"wal"},
	{"threshold"},
	{"repo", "bundle"},
	{"llm", "history"},
}

// TestGroupGuard_RejectsUnknownSubcommand is the behaviour the whole
// mechanism exists for.
func TestGroupGuard_RejectsUnknownSubcommand(t *testing.T) {
	for _, group := range groupsUnderTest {
		path := strings.Join(group, " ")
		t.Run(strings.ReplaceAll(path, " ", "_"), func(t *testing.T) {
			args := append(append([]string{}, group...), "definitely-not-a-subcommand")
			exit, out := runGroup(t, args...)

			if exit == 0 {
				t.Fatalf("`%s definitely-not-a-subcommand` exited 0.\nA mistyped subcommand "+
					"must fail: in a cron entry or a runbook, exit 0 with a help screen "+
					"reads as a successful run, and the backup nobody took goes unnoticed "+
					"until someone looks in the repo.\noutput:\n%s", path, out)
			}
			if exit != int(output.ExitMisuse) {
				t.Errorf("exit = %d, want %d (misuse). Scripts distinguish \"I called it "+
					"wrong\" from \"it ran and failed\"", exit, int(output.ExitMisuse))
			}
			if !strings.Contains(out, "unknown subcommand") {
				t.Errorf("output does not say what was wrong:\n%s", out)
			}
		})
	}
}

// TestGroupGuard_BareGroupPrintsHelp is the other half. The guard must
// reject typos WITHOUT breaking the ordinary "what can I do here?"
// invocation — which is also the reason these commands are exempt from
// the coverage gate, so if this stopped holding the exemption would be
// wrong too.
func TestGroupGuard_BareGroupPrintsHelp(t *testing.T) {
	for _, group := range groupsUnderTest {
		path := strings.Join(group, " ")
		t.Run(strings.ReplaceAll(path, " ", "_"), func(t *testing.T) {
			exit, out := runGroup(t, group...)
			if exit != 0 {
				t.Errorf("`%s` exited %d; a bare group must print help and succeed",
					path, exit)
			}
			if !strings.Contains(out, "Usage:") {
				t.Errorf("`%s` printed no usage:\n%s", path, out)
			}
		})
	}
}

// TestGroupGuard_SuggestsNearMisses covers the affordance the guard
// adds over a bare rejection. A near-miss is the likely real-world
// typo, and the suggestion is what turns the failure into a fix.
func TestGroupGuard_SuggestsNearMisses(t *testing.T) {
	// "rotat" is one deletion away from `kms rotate`.
	exit, out := runGroup(t, "kms", "rotat")
	if exit == 0 {
		t.Fatalf("`kms rotat` exited 0:\n%s", out)
	}
	if !strings.Contains(out, "did you mean") || !strings.Contains(out, "rotate") {
		t.Errorf("no suggestion for a one-character typo; the operator gets a rejection "+
			"with nothing to act on:\n%s", out)
	}
}
