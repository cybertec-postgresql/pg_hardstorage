package fs

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// TestReapStaging_RemovesStaleTempsOnly pins M32: staging temps a crash
// left behind are hidden from List, so no GC ever saw them and the
// disk leaked forever. The reaper removes backend staging temps older
// than the cut-off and nothing else.
func TestReapStaging_RemovesStaleTempsOnly(t *testing.T) {
	root := t.TempDir()
	p := openAt(t, root)
	ctx := context.Background()
	if _, err := p.Put(ctx, "chunks/ab/cd/real", bytes.NewReader([]byte("keep")), storage.PutOptions{IfNotExists: true}); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	write := func(rel string, age time.Time) string {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("leaked-bytes"), 0o640); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(full, age, age); err != nil {
			t.Fatal(err)
		}
		return full
	}
	stale := []string{
		write("chunks/ab/cd/x.hstmp-0123456789abcdef", old),
		write("chunks/ab/cd/y.deferred-0123456789abcdef", old),
		write("manifests/db1/m.json.excl-0123456789abcdef", old),
	}
	keep := []string{
		write("chunks/ab/cd/z.hstmp-fedcba9876543210", time.Now()), // live writer
		write("manifests/db1/b.json.tmp.0123456789abcdef", old),    // caller's key, own reaper
		write("chunks/ab/cd/legacy.tmp", old),                      // unknown owner
		filepath.Join(root, "chunks/ab/cd/real"),
	}

	st, err := storage.ReapStagingOf(ctx, p, storage.DefaultStagingReapAge)
	if err != nil {
		t.Fatalf("ReapStagingOf: %v", err)
	}
	if st.Unsupported {
		t.Fatal("fs reported staging reap unsupported; leaked temps can never be removed")
	}
	if st.Removed != len(stale) || st.Bytes != int64(len(stale)*len("leaked-bytes")) {
		t.Errorf("stats = %+v, want %d removed / %d bytes", st, len(stale), len(stale)*len("leaked-bytes"))
	}
	for _, f := range stale {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("stale staging temp %s survived", f)
		}
	}
	for _, f := range keep {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("%s must not be reaped: %v", f, err)
		}
	}

	if _, err := storage.ReapStagingOf(ctx, p, time.Minute); err == nil {
		t.Error("an age below the safety floor was accepted")
	}
}

// TestReapStaging_SparesThisProcessPendingDeferred: a deferred chunk
// staged by THIS process and not yet published by Barrier must survive
// even when old — reaping it would make the Barrier fail.
func TestReapStaging_SparesThisProcessPendingDeferred(t *testing.T) {
	root := t.TempDir()
	p := openAt(t, root)
	ctx := context.Background()
	if _, err := p.Put(ctx, "chunks/ab/cd/d", bytes.NewReader([]byte("x")),
		storage.PutOptions{IfNotExists: true, Durability: storage.DurabilityDeferred}); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	p.mu.Lock()
	staged := p.deferred[0].staging
	p.mu.Unlock()
	if err := os.Chtimes(staged, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ReapStaging(ctx, storage.DefaultStagingReapAge); err != nil {
		t.Fatal(err)
	}
	if err := p.Barrier(ctx); err != nil {
		t.Fatalf("Barrier after reap: %v (the reaper removed a pending deferred chunk)", err)
	}
}
