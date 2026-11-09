//go:build linux

package fs

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// Issue #90: a deferred create skips the per-directory fsyncs — the
// Barrier's syncfs persists the new directory entries — so a first
// backup no longer pays two journal commits per new chunk bucket. The
// chunk still only appears at its key once the Barrier has run.
func TestPut_DeferredCreateSkipsDirSyncUntilBarrier(t *testing.T) {
	root := t.TempDir()
	p := openAt(t, root)
	synced := recordDirSyncs(t)
	key := "chunks/sha256/ab/cd/k.chk"
	if _, err := p.Put(context.Background(), key, bytes.NewReader([]byte("x")),
		storage.PutOptions{IfNotExists: true, Durability: storage.DurabilityDeferred}); err != nil {
		t.Fatal(err)
	}
	if got := synced(); len(got) != 0 {
		t.Fatalf("deferred Put fsynced directories %v; the Barrier's syncfs covers them", got)
	}
	if _, err := os.Stat(filepath.Join(root, key)); !os.IsNotExist(err) {
		t.Fatalf("chunk visible at its key before the Barrier (err=%v)", err)
	}
	if err := p.Barrier(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(root, key)); err != nil || string(b) != "x" {
		t.Fatalf("after Barrier: %q, %v", b, err)
	}
}
