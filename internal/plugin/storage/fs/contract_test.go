// Contract suite for the fs plugin.  Pure-Go, no Docker —
// runs on every PR with the rest of the unit tests.  When a
// new contract clause lands in internal/plugin/storage/contract,
// every plugin's Run call exercises it; if fs ever drifts
// from the documented behaviour, this file fails immediately.
package fs_test

import (
	"context"
	"net/url"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/contract"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
)

func TestFS_Contract(t *testing.T) {
	contract.Run(t, func(t *testing.T) storage.StoragePlugin {
		root := t.TempDir()
		p := &fs.Plugin{}
		if err := p.Open(context.Background(), storage.StorageConfig{
			URL: &url.URL{Scheme: "file", Path: root},
		}); err != nil {
			t.Fatalf("fs.Open: %v", err)
		}
		t.Cleanup(func() { _ = p.Close() })
		return p
	})
}

// TestFS_Contract_ParallelPuts exercises the IfNotExists
// concurrent-writers clause separately — it's an opt-in
// extra to keep the core contract suite small and uniform
// across backends that may not yet meet it.
func TestFS_Contract_ParallelPuts(t *testing.T) {
	contract.ParallelPuts(t, func(t *testing.T) storage.StoragePlugin {
		root := t.TempDir()
		p := &fs.Plugin{}
		if err := p.Open(context.Background(), storage.StorageConfig{
			URL: &url.URL{Scheme: "file", Path: root},
		}); err != nil {
			t.Fatalf("fs.Open: %v", err)
		}
		t.Cleanup(func() { _ = p.Close() })
		return p
	}, 16)
}

// TestFS_Contract_ParallelOverwrites pins the putOverwrite concurrency
// fix: concurrent overwrites of the same key must publish ONE complete
// body, never a torn mix (the bug was a fixed "<full>.tmp" staging path
// shared by all writers).
func TestFS_Contract_ParallelOverwrites(t *testing.T) {
	contract.ParallelOverwrites(t, func(t *testing.T) storage.StoragePlugin {
		root := t.TempDir()
		p := &fs.Plugin{}
		if err := p.Open(context.Background(), storage.StorageConfig{
			URL: &url.URL{Scheme: "file", Path: root},
		}); err != nil {
			t.Fatalf("fs.Open: %v", err)
		}
		t.Cleanup(func() { _ = p.Close() })
		return p
	}, 16)
}
