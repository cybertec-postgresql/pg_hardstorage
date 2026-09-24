package repo

import (
	"context"
	"fmt"
	"net/url"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
)

// GetChunkBytes caches every verified read in the positive cache. It
// used to Store straight into the map, bypassing markSeen's bound: a
// restore reads every chunk of a backup through one CAS, so the cache
// grew one entry per chunk read — O(chunks) memory on a large restore,
// the leak the bound exists to prevent. And because those entries were
// never counted, a later DeleteChunk of one drove seenCount negative,
// silently raising the effective cap.
func TestCAS_GetChunkBytesRespectsSeenBound(t *testing.T) {
	ctx := context.Background()
	sp := &fs.Plugin{}
	if err := sp.Open(ctx, storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: t.TempDir()}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })

	// A writer CAS fills the repo; a separate reader CAS (a restore)
	// starts with an empty cache.
	w := NewCAS(sp, WithSeenCacheLimit(0))
	var hashes []Hash
	for i := 0; i < 200; i++ {
		ci, err := w.PutChunk(ctx, []byte(fmt.Sprintf("chunk-%d", i)))
		if err != nil {
			t.Fatal(err)
		}
		hashes = append(hashes, ci.Hash)
	}

	const limit = 16
	r := NewCAS(sp, WithSeenCacheLimit(limit))
	for _, h := range hashes {
		if _, err := r.GetChunkBytes(ctx, h); err != nil {
			t.Fatal(err)
		}
	}
	if n := seenLen(r); n > limit {
		t.Errorf("reader cache holds %d entries after reading %d chunks; the bound is %d", n, len(hashes), limit)
	}

	// Count must stay non-negative through deletes of read-cached entries.
	r2 := NewCAS(sp, WithSeenCacheLimit(1000))
	for _, h := range hashes[:5] {
		if _, err := r2.GetChunkBytes(ctx, h); err != nil {
			t.Fatal(err)
		}
	}
	for _, h := range hashes[:5] {
		if err := r2.DeleteChunk(ctx, h); err != nil {
			t.Fatal(err)
		}
	}
	if got := r2.seenCount.Load(); got != 0 {
		t.Errorf("seenCount = %d after caching and deleting 5 chunks, want 0", got)
	}
}
