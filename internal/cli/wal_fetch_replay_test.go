package cli_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// An unsigned segment manifest copied over another segment's key (same
// repo write access an attacker needs anyway) must not be served.
func TestWalFetch_SwappedSegmentManifest_Refused(t *testing.T) {
	repoURL := initRepoForTest(t)
	a := pushSegAs(t, repoURL, seedSysID, 1, 5)
	b := pushSegAs(t, repoURL, seedSysID, 1, 6)
	_, sp, err := repo.Open(context.Background(), repoURL)
	if err != nil {
		t.Fatal(err)
	}
	keyA := walsink.SegmentPath("db1", 1, a)
	keyB := walsink.SegmentPath("db1", 1, b)
	_ = sp.Close()
	root := repoURL[len("file://"):]
	body, err := os.ReadFile(filepath.Join(root, keyA))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, keyB), body, 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "out.wal")
	_, stderr, exit := runCmd(t, "wal", "fetch", "db1", b, target, "--repo", repoURL, "--output", "json")
	if exit == 0 {
		t.Fatalf("served segment %s from the manifest of %s", b, a)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("refused fetch wrote a file: %v (%s)", err, stderr)
	}
}
