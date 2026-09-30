package repo

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A crashed writer leaves backend staging files (.deferred-/.excl-/
// .hstmp-) that List hides, so nothing but the reaper can see them.
// `repo gc --apply` must remove old ones and leave a fresh one (an
// in-flight write) alone.
func TestSweep_ApplyReapsOldBackendStagingFiles(t *testing.T) {
	sp, root := fenceWorld(t)
	dir := filepath.Join(root, "chunks", "sha256", "ab", "cd")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	oldTmp := filepath.Join(dir, "abcd.chk.deferred-crashed")
	freshTmp := filepath.Join(dir, "abcd.chk.deferred-inflight")
	for _, f := range []string{oldTmp, freshTmp} {
		if err := os.WriteFile(f, []byte("partial chunk body"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(oldTmp, old, old); err != nil {
		t.Fatal(err)
	}

	res, err := Sweep(context.Background(), sp, SweepOptions{Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.StagingReaped != 1 {
		t.Errorf("StagingReaped = %d, want 1", res.StagingReaped)
	}
	if _, err := os.Stat(oldTmp); !os.IsNotExist(err) {
		t.Errorf("crash-leftover staging file survived gc --apply: %v", err)
	}
	if _, err := os.Stat(freshTmp); err != nil {
		t.Errorf("gc removed a fresh staging file (an in-flight write): %v", err)
	}
}
