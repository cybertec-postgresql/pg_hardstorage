package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// H24: `integrity run` persisted and signed a run whose status was
// "error" and then exited 0 — the cron job asking "did the check pass"
// was told yes. An errored run is still recorded (clearly marked), but
// the exit code must be non-zero.
func TestIntegrityRun_ErrorStatusExitsNonZero(t *testing.T) {
	w := newReadWorld(t)
	commitMinimalManifest(t, w, "db1", "x", 2)

	// Make every chunk Stat fail with EACCES — neither "present" nor
	// "not found": the run cannot complete its presence check.
	root := strings.TrimPrefix(w.repoURL, "file://")
	chunks := filepath.Join(root, "chunks")
	if err := os.Chmod(chunks, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(chunks, 0o755) })

	stdout, errb, exit := runCLI(t, "integrity", "run",
		"--repo", w.repoURL, "--strategy", "presence", "-o", "json")
	if exit == int(output.ExitOK) {
		t.Fatalf("an errored integrity run exited 0\n%s", stdout)
	}
	if exit == int(output.ExitVerifyFailed) {
		t.Fatalf("a run that could not check presence reported verified integrity issues (exit 9)\n%s", errb)
	}
	if !strings.Contains(errb, "integrity.run_incomplete") {
		t.Errorf("expected integrity.run_incomplete:\n%s", errb)
	}
	var v integrityRunView
	bodyOf(t, stdout, &v)
	if v.Status != "error" || v.Chunks.Missing != 0 {
		t.Errorf("status=%q missing=%d; want error/0", v.Status, v.Chunks.Missing)
	}
}
