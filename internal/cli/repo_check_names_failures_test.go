package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// A manifest that fails to parse or verify — here an old-schema (v0.8)
// shape planted next to a real backup — must be refused (exit 9) AND
// named. repo check used to return the bare count "1 manifest
// signature(s) failed verification", leaving the operator to find the
// culprit among every manifest in the repository.
func TestRepoCheck_NamesManifestsThatFailVerification(t *testing.T) {
	w := newReadWorld(t)
	commitVerifiableBackup(t, w, "db1", 0, []byte("real backup body"))
	root := strings.TrimPrefix(w.repoURL, "file://")
	p := filepath.Join(root, "manifests", "db1", "backups", "synthetic-v0_8", "manifest.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`{"schema_version":"v0.8","type":"synthetic.placeholder"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, exit := runCLI(t, "repo", "check", "--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitVerifyFailed) {
		t.Fatalf("exit = %d, want %d\n%s\n%s", exit, output.ExitVerifyFailed, stdout, stderr)
	}
	if !strings.Contains(stdout, `"signature_failed_manifests"`) || !strings.Contains(stdout, "synthetic-v0_8") {
		t.Errorf("result body does not name the failing manifest:\n%s", stdout)
	}
	if !strings.Contains(stderr, "synthetic-v0_8") {
		t.Errorf("error message does not name the failing manifest:\n%s", stderr)
	}
}
