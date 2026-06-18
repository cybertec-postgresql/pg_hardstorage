package repo_test

import (
	"context"
	"fmt"
	"net/url"
	"sync"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

func hintHash(i int) repo.Hash {
	var h repo.Hash
	h[0], h[1], h[2] = byte(i), byte(i>>8), byte(i>>16)
	return h
}

// TestCAS_DedupHintsIsolatedFromCaller pins data-race audit #5:
// WithDedupHints copies the caller's map, so PutChunk's lock-free reads
// of the hint set (it runs concurrently across the base-backup chunk
// worker pool) can't race a caller that keeps mutating the map it passed.
// Run under -race; against the unfixed option (which stored the caller's
// map by reference) this reports a concurrent map read/write.
func TestCAS_DedupHintsIsolatedFromCaller(t *testing.T) {
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: t.TempDir()}}); err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = sp.Close() })

	hints := map[repo.Hash]struct{}{hintHash(0): {}}
	c := repo.NewCAS(sp, repo.WithDedupHints(hints))

	var wg sync.WaitGroup

	// Caller keeps mutating the ORIGINAL map after construction.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 1; i <= 100_000; i++ {
			hints[hintHash(i)] = struct{}{}
		}
	}()

	// Worker pool: every PutChunk reads the CAS's hint set on its hint
	// path. With the defensive copy that's a different map than the one
	// the caller mutates above.
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			ctx := context.Background()
			for i := 0; i < 4000; i++ {
				if _, err := c.PutChunk(ctx, []byte(fmt.Sprintf("w%d-chunk-%d", w, i))); err != nil {
					t.Errorf("PutChunk: %v", err)
					return
				}
			}
		}(w)
	}

	wg.Wait()
}
