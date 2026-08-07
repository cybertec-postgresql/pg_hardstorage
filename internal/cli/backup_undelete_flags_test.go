package cli_test

// backup_undelete_flags_test.go — what each undelete flag actually
// gates, pinned after being misdocumented twice.
//
// The layering, verified against the code and against a repo whose
// chunks were genuinely reclaimed:
//
//   - PLAIN undelete is safe by default. The per-ID store call
//     (ManifestStore.Undelete) fails closed on missing chunks with
//     conflict.chunks_missing, whether or not any flag was passed.
//   - --check-chunks is the BATCH pre-pass on top: a multi-ID undelete
//     refuses atomically before touching anything, and --skip-missing
//     can then select the survivors. It is NOT the only safety.
//   - --force bypasses BOTH (routes to UndeleteForce), to recover the
//     metadata of a backup that can no longer restore. Forensic use.
//
// The history matters because both prior descriptions were wrong in
// opposite directions. The original --force help said it "skips the
// restorability pre-flight", implying a pre-flight ran by default; a
// "fix" then declared --force a no-op without --check-chunks and
// emitted a warning claiming manifests were "resurrected without any
// restorability check" — false, the store checks unconditionally. That
// warning shipped because the reader stopped at the batch-gate
// condition (`checkChunks && !force`) and never reached the per-ID
// loop where force actually switches the store call. These tests exist
// so the next reader cannot repeat either mistake without going red.

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// TestUndelete_SkipMissingRequiresCheckChunks: --skip-missing changes
// batch semantics that only exist under the pre-pass, so alone it is a
// usage error.
func TestUndelete_SkipMissingRequiresCheckChunks(t *testing.T) {
	t.Setenv("PG_HARDSTORAGE_KEYRING_DIR", t.TempDir())
	repoURL := initRepoForTest(t)

	_, errb, exit := runCLI(t, "backup", "undelete", "db1", "x",
		"--repo", repoURL, "--skip-missing", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("--skip-missing without --check-chunks exited %d, want ExitMisuse (%d):\n%s",
			exit, int(output.ExitMisuse), errb)
	}
}

// TestUndelete_ForceAloneIsAccepted: --force is meaningful without
// --check-chunks — it bypasses the store's own fail-closed check — so
// it must be accepted silently. The behavioural proof that it actually
// bypasses lives in TestTombstoneWindow_ForceRecoversMetadataOnly,
// against a repo whose chunks are really gone.
func TestUndelete_ForceAloneIsAccepted(t *testing.T) {
	t.Setenv("PG_HARDSTORAGE_KEYRING_DIR", t.TempDir())
	repoURL := initRepoForTest(t)

	outb, errb, _ := runCLI(t, "backup", "undelete", "db1", "no-such-backup",
		"--repo", repoURL, "--force", "-o", "json")
	combined := outb + errb
	if strings.Contains(combined, "force_without_check") {
		t.Errorf("the retracted force_without_check warning is back:\n%s\n\n"+
			"It claimed --force 'had no effect' without --check-chunks and that manifests "+
			"were 'resurrected without any restorability check'. Both clauses are false: "+
			"--force switches the per-ID call to UndeleteForce, and without it the store "+
			"fails closed on missing chunks unconditionally.", combined)
	}
}
