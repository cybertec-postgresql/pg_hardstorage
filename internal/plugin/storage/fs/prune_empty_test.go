package fs

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

func openPruneFS(t *testing.T) (*Plugin, string) {
	t.Helper()
	root := t.TempDir()
	p := &Plugin{}
	if err := p.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: root}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p, root
}

func TestPruneEmptyDirs_Basic(t *testing.T) {
	p, root := openPruneFS(t)
	ctx := context.Background()
	_, _ = p.PutBytes(ctx, "chunks/sha256/aa/bb/keep", []byte("x"), storage.PutOptions{})
	_, _ = p.PutBytes(ctx, "chunks/sha256/cc/dd/gone", []byte("x"), storage.PutOptions{})
	_ = p.Delete(ctx, "chunks/sha256/cc/dd/gone")
	n, err := p.PruneEmptyDirs(ctx, "chunks", false)
	if err != nil || n != 2 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if _, err := os.Stat(filepath.Join(root, "chunks/sha256/cc")); err == nil {
		t.Fatal("empty dir survived")
	}
	if _, err := os.Stat(filepath.Join(root, "chunks/sha256/aa/bb/keep")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "chunks")); err != nil {
		t.Fatal("prefix itself must stay")
	}
}

// Puts racing the prune must never fail because their directory vanished.
func TestPruneEmptyDirs_RacesPut(t *testing.T) {
	p, _ := openPruneFS(t)
	ctx := context.Background()
	stop := make(chan struct{})
	var pw sync.WaitGroup
	pw.Add(1)
	go func() {
		defer pw.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = p.PruneEmptyDirs(ctx, "chunks", false)
			}
		}
	}()
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 400; i++ {
				key := fmt.Sprintf("chunks/sha256/%02x/%02x/g%d-%d", i%7, i%3, g, i)
				_, err := p.PutBytes(ctx, key, []byte("x"), storage.PutOptions{IfNotExists: i%2 == 0})
				if err != nil {
					t.Errorf("put %s: %v", key, err)
					return
				}
				if err := p.Delete(ctx, key); err != nil {
					t.Errorf("delete: %v", err)
				}
				if i%5 == 0 {
					if _, err := p.PutBytes(ctx, key+"-src", []byte("x"), storage.PutOptions{}); err == nil {
						if err := p.RenameIfNotExists(ctx, key+"-src", key+"-dst"); err != nil {
							t.Errorf("rename: %v", err)
						}
					}
				}
			}
		}(g)
	}
	wg.Wait()
	close(stop)
	pw.Wait()
}
