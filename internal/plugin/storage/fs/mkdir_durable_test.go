package fs

import (
	"bytes"
	"context"
	"net/url"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

func recordDirSyncs(t *testing.T) func() []string {
	t.Helper()
	var mu sync.Mutex
	var got []string
	dirSyncHook = func(dir string) {
		mu.Lock()
		got = append(got, filepath.Clean(dir))
		mu.Unlock()
	}
	t.Cleanup(func() { dirSyncHook = nil })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

func openAt(t *testing.T, root string) *Plugin {
	t.Helper()
	u, _ := url.Parse("file://" + root)
	p := &Plugin{}
	if err := p.Open(context.Background(), storage.StorageConfig{URL: u}); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestPut_NewDirectoriesAreSyncedInTheirParents pins the LOW finding:
// Put's MkdirAll created missing directories (e.g. a fresh chunk
// bucket chunks/sha256/ab/cd) but only the LEAF directory was ever
// fsynced. The new directories' own entries live in their parents; a
// power loss could drop a whole new chunk directory — and every chunk
// in it — although each Put had reported durable success.
func TestPut_NewDirectoriesAreSyncedInTheirParents(t *testing.T) {
	for _, tc := range []struct {
		name string
		put  func(p *Plugin) error
	}{
		{"exclusive", func(p *Plugin) error {
			_, err := p.Put(context.Background(), "a/b/c/k", bytes.NewReader([]byte("x")), storage.PutOptions{IfNotExists: true})
			return err
		}},
		{"overwrite", func(p *Plugin) error {
			_, err := p.Put(context.Background(), "a/b/c/k", bytes.NewReader([]byte("x")), storage.PutOptions{})
			return err
		}},
		{"rename", func(p *Plugin) error {
			if _, err := p.Put(context.Background(), "src", bytes.NewReader([]byte("x")), storage.PutOptions{}); err != nil {
				return err
			}
			return p.RenameIfNotExists(context.Background(), "src", "a/b/c/k")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			p := openAt(t, root)
			synced := recordDirSyncs(t)
			if err := tc.put(p); err != nil {
				t.Fatal(err)
			}
			got := synced()
			for _, parent := range []string{root, filepath.Join(root, "a"), filepath.Join(root, "a", "b")} {
				if !slices.Contains(got, parent) {
					t.Errorf("new child directory's entry not synced in %s (synced: %v)", parent, got)
				}
			}
		})
	}
}
