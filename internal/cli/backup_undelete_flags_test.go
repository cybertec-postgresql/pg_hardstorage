package cli_test

// backup_undelete_flags_test.go — the restorability pre-flight is
// opt-in, and --force must not pretend otherwise.
//
// `backup undelete` carries two flags about chunk checking:
//
//   --check-chunks  opt IN to a Stat pass over every referenced chunk
//   --force         override that pass and resurrect anyway
//
// Because the pass is opt-in, --force on its own overrides nothing —
// undelete already resurrects regardless of chunk state. Its help text
// said it "skips the restorability pre-flight", which reads as though a
// pre-flight runs by default and --force turns it off. An operator
// reaching for it during an incident would reasonably conclude the
// resurrected backup had been checked and deliberately forced, when in
// fact nothing checked anything.
//
// That matters because the window this command exists for is exactly
// the one where chunks may already be gone: `repo gc --apply` reclaims
// them once the tombstone grace expires, and undelete after that
// succeeds while producing a backup that cannot restore.

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// TestUndelete_ForceWithoutCheckChunksWarns: the flag is accepted —
// it asks for exactly the behaviour it gets — but the operator is told
// it changed nothing and that no restorability check ran.
func TestUndelete_ForceWithoutCheckChunksWarns(t *testing.T) {
	t.Setenv("PG_HARDSTORAGE_KEYRING_DIR", t.TempDir())
	repoURL := initRepoForTest(t)

	// A backup id that does not exist is fine: the warning is emitted
	// from flag handling, before any manifest is touched.
	// Events render on STDOUT in JSON mode; stderr carries errors.
	outb, _, _ := runCLI(t, "backup", "undelete", "db1", "no-such-backup",
		"--repo", repoURL, "--force", "-o", "json")

	if !strings.Contains(outb, "force_without_check") {
		t.Errorf("--force without --check-chunks emitted no warning:\n%s\n\n"+
			"The pre-flight it overrides is opt-in, so the flag did nothing. Accepting it "+
			"silently lets an operator believe the resurrected backup was checked and "+
			"deliberately forced.", outb)
	}
	for _, want := range []string{"--check-chunks", "fail at restore"} {
		if !strings.Contains(outb, want) {
			t.Errorf("the warning does not mention %q:\n%s", want, outb)
		}
	}
}

// TestUndelete_PlainInvocationDoesNotWarn: no --force, no noise. This
// runs on the ordinary path, and a warning here would be pure noise.
func TestUndelete_PlainInvocationDoesNotWarn(t *testing.T) {
	t.Setenv("PG_HARDSTORAGE_KEYRING_DIR", t.TempDir())
	repoURL := initRepoForTest(t)

	outb, _, _ := runCLI(t, "backup", "undelete", "db1", "no-such-backup",
		"--repo", repoURL, "-o", "json")
	if strings.Contains(outb, "force_without_check") {
		t.Errorf("warned without --force being passed:\n%s", outb)
	}
}

// TestUndelete_SkipMissingStillRequiresCheckChunks pins the asymmetry
// deliberately. --skip-missing changes batch semantics that only exist
// under the pre-flight, so it IS a usage error; --force asks for what
// it gets, so it is a warning. Both behaviours are intentional and
// neither should drift into the other.
func TestUndelete_SkipMissingStillRequiresCheckChunks(t *testing.T) {
	t.Setenv("PG_HARDSTORAGE_KEYRING_DIR", t.TempDir())
	repoURL := initRepoForTest(t)

	_, errb, exit := runCLI(t, "backup", "undelete", "db1", "x",
		"--repo", repoURL, "--skip-missing", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("--skip-missing without --check-chunks exited %d, want ExitMisuse (%d):\n%s",
			exit, int(output.ExitMisuse), errb)
	}
}
