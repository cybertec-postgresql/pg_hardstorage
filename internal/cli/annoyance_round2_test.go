package cli_test

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// Bug #13: a typo'd subcommand under a group (`wal audi`, `repo bogus`)
// used to print the group help and exit 0 — cron/CI stayed green forever.
// It must be a usage error (exit 2). Bare groups still help+0.
func TestGroupCommands_UnknownSubcommandFails(t *testing.T) {
	for _, args := range [][]string{
		{"wal", "audi"},
		{"repo", "bogus"},
		{"kms", "nonsense"},
	} {
		_, _, exit := runCLI(t, args...)
		if exit != int(output.ExitMisuse) {
			t.Errorf("%v exit = %d, want %d (usage)", args, exit, output.ExitMisuse)
		}
	}
	// Bare group: conventional help, exit 0.
	_, _, exit := runCLI(t, "wal")
	if exit != 0 {
		t.Errorf("bare `wal` exit = %d, want 0 (help)", exit)
	}
}

// Bug #14: an empty backup-ID positional (unset shell variable) was
// reported as a manifest SIGNATURE failure (exit 9) by verify and as an
// internal error (exit 1) by show. It is a usage error (exit 2).
func TestEmptyBackupID_IsUsageError(t *testing.T) {
	for _, args := range [][]string{
		{"verify", "db1", "", "--repo", "file:///nonexistent"},
		{"show", "db1", "", "--repo", "file:///nonexistent"},
		{"restore", "db1", "", "--repo", "file:///nonexistent", "--target", t.TempDir()},
	} {
		out, errOut, exit := runCLI(t, args...)
		if exit != int(output.ExitMisuse) {
			t.Errorf("%v exit = %d, want %d (usage); out=%s err=%s", args, exit, output.ExitMisuse, out, errOut)
		}
	}
}

// Bug #18: `backup delete` (the most destructive verb) executed with no
// confirmation while config-only `deployment remove` required --yes. It
// now refuses without --yes / --require-approval.
func TestBackupDelete_RequiresConfirmation(t *testing.T) {
	out, errOut, exit := runCLI(t,
		"backup", "delete", "db1", "some.full.id",
		"--repo", "file:///nonexistent", "-o", "json")
	combined := out + errOut
	if !strings.Contains(combined, "aborted.confirmation_required") {
		t.Errorf("expected aborted.confirmation_required, got exit=%d out=%s", exit, combined)
	}
}
