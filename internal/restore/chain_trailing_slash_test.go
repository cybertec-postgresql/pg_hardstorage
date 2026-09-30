package restore_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
)

// Regression (M98): `--target /x/merged/` (trailing slash) made the
// chain path derive its staging dir as "/x/merged/.pgcombine-staging"
// — INSIDE the target — so the final rename(staging, target) failed
// with EINVAL and the merged output was deleted.
func TestRestore_ChainTargetWithTrailingSlash(t *testing.T) {
	fx := newChainFixture(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "merged")
	fakeCombine(t, dir, fx.inc.PGVersion, "")

	if _, err := restore.Restore(context.Background(), restore.Options{
		RepoURL:    fx.repoURL,
		Deployment: "db1",
		BackupID:   fx.inc.BackupID,
		TargetDir:  target + string(os.PathSeparator),
		Verifier:   fx.verifier,
	}); err != nil {
		t.Fatalf("chain restore to %q: %v", target+"/", err)
	}
	if _, err := os.Stat(filepath.Join(target, "PG_VERSION")); err != nil {
		t.Errorf("merged datadir not finalized at the target: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, ".pgcombine-staging")); err == nil {
		t.Errorf("staging dir left inside the target")
	}
}
