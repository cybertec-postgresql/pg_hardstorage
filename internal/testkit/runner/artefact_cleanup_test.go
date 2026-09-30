package runner

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func ownedDir(t *testing.T) string {
	t.Helper()
	d := filepath.Join(t.TempDir(), "pg_hardstorage-testkit-x")
	if err := os.MkdirAll(filepath.Join(d, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "result.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	return d
}

// Nothing removed the runner's own temp dirs, and ~1,800 of them
// accumulated in $TMPDIR over one release's scenario campaigns.
func TestRemoveOwnedArtefactDir_PassRemoves(t *testing.T) {
	d := ownedDir(t)
	removeOwnedArtefactDir(d, true, "tear_down", io.Discard)
	if _, err := os.Stat(d); !os.IsNotExist(err) {
		t.Fatalf("a passing scenario's temp dir must be removed; stat err=%v", err)
	}
	d = ownedDir(t)
	removeOwnedArtefactDir(d, true, "", io.Discard) // default is tear_down
	if _, err := os.Stat(d); !os.IsNotExist(err) {
		t.Fatalf("the default on_success must remove the dir; stat err=%v", err)
	}
}

func TestRemoveOwnedArtefactDir_FailureOrKeepRetains(t *testing.T) {
	for _, tc := range []struct {
		pass      bool
		onSuccess string
	}{{false, "tear_down"}, {false, ""}, {true, "keep"}} {
		d := ownedDir(t)
		removeOwnedArtefactDir(d, tc.pass, tc.onSuccess, io.Discard)
		if _, err := os.Stat(filepath.Join(d, "result.json")); err != nil {
			t.Errorf("pass=%v on_success=%q: dir must be kept for triage: %v", tc.pass, tc.onSuccess, err)
		}
	}
}
