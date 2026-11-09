package fs

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// A writer whose directory was pruned re-creates it and tries again.
// It used to re-create it DURABLY — mkdir, then an fsync per new
// component — before going back to create its file, and on a busy disk
// those fsyncs take long enough for a prune that is still running to
// remove the empty directory again, every time: the writer lost a
// hundred rounds in a row and failed with "no such file or directory"
// (release validation, IO pressure above 50%). The directory fsync is
// slowed down here to make that disk; the writers must still get
// through.
func TestPruneEmptyDirs_RacesPut_SlowDirectorySync(t *testing.T) {
	p, _ := openPruneFS(t)
	dirSyncHook = func(string) { time.Sleep(20 * time.Millisecond) }
	t.Cleanup(func() { dirSyncHook = nil })

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
	// One writer: several of them re-create the directory for each other
	// often enough to hide the starvation.
	for g := 0; g < 1; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				key := fmt.Sprintf("chunks/sha256/%02x/%02x/g%d-%d", i%7, i%3, g, i)
				if _, err := p.PutBytes(ctx, key, []byte("x"), storage.PutOptions{IfNotExists: i%2 == 0}); err != nil {
					t.Errorf("put %s: %v", key, err)
					return
				}
				if err := p.Delete(ctx, key); err != nil {
					t.Errorf("delete: %v", err)
				}
				if i%5 == 0 {
					if _, err := p.PutBytes(ctx, key+"-src", []byte("x"), storage.PutOptions{}); err != nil {
						t.Errorf("put src: %v", err)
						return
					}
					if err := p.RenameIfNotExists(ctx, key+"-src", key+"-dst"); err != nil {
						t.Errorf("rename: %v", err)
					}
				}
			}
		}(g)
	}
	wg.Wait()
	close(stop)
	pw.Wait()
}
