package cli_test

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// Regression: `hold remove --yes` on a hold that never existed printed
// "✓ Hold released" with exit 0 (the storage layer's RemoveHold is
// idempotent) — a false success on the legal-hold path: typo one
// character of the backup ID and the real hold silently keeps blocking
// retention. It must fail with notfound.hold.
func TestHoldRemove_NonexistentHoldFails(t *testing.T) {
	repoURL := "file://" + t.TempDir() + "/repo"
	if _, _, exit := runCLI(t, "repo", "init", repoURL); exit != int(output.ExitOK) {
		t.Fatalf("repo init failed")
	}
	out, errOut, exit := runCLI(t,
		"hold", "remove", "db1", "db1.full.20990101T000000Z.dead",
		"--repo", repoURL, "--yes", "-o", "json")
	combined := out + errOut
	if exit == 0 {
		t.Fatalf("hold remove on nonexistent hold exited 0 (false success):\n%s", combined)
	}
	if !strings.Contains(combined, "notfound.hold") {
		t.Errorf("expected notfound.hold, got exit=%d:\n%s", exit, combined)
	}
}
