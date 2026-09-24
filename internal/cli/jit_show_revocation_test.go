package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// TestJitShow_RevocationReadErrorNotReportedActive pins that `jit show`
// distinguishes "no revocation marker" from "could not read the
// marker". The read error used to be discarded, so a token whose
// marker existed but could not be read rendered as active -- the
// permissive answer, on the surface an operator uses to confirm a
// break-glass revocation took effect.
func TestJitShow_RevocationReadErrorNotReportedActive(t *testing.T) {
	w := newReadWorld(t)
	id := mustIssue(t, w, "ops@acme.example", "kms.shred", "default")

	// A directory at the marker key: Get fails with an I/O error that
	// is not ErrNotFound.
	root := strings.TrimPrefix(w.repoURL, "file://")
	if err := os.MkdirAll(filepath.Join(root, "jit", id+".json.revoked"), 0o755); err != nil {
		t.Fatal(err)
	}

	stdout, errb, exit := runCLI(t, "jit", "show", id, "--repo", w.repoURL, "-o", "json")
	if exit == int(output.ExitOK) {
		t.Fatalf("exit = 0 with an unreadable revocation marker\n%s", stdout)
	}
	if strings.Contains(stdout, `"effective_status": "active"`) {
		t.Errorf("token reported active despite unreadable revocation marker:\n%s", stdout)
	}
	if !strings.Contains(stdout, `"effective_status": "unknown"`) {
		t.Errorf("want effective_status unknown:\n%s", stdout)
	}
	if !strings.Contains(errb, "jit.revocation_unknown") {
		t.Errorf("want jit.revocation_unknown:\n%s", errb)
	}
}
