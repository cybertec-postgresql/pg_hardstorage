package repo

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/url"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
)

// seenLen counts the entries currently held in the positive cache.
func seenLen(c *CAS) int {
	n := 0
	c.seen.Range(func(_, _ any) bool { n++; return true })
	return n
}

func hashOfInt(i int) Hash {
	var h Hash
	h[0] = byte(i)
	h[1] = byte(i >> 8)
	h[2] = byte(i >> 16)
	return h
}

// TestCAS_MarkSeenBounded pins the memory bound on the positive cache
// (memory-leak audit #1): inserting far more distinct hashes than the
// cap must leave the cache at or below the cap, because it is cleared
// wholesale on overflow. Without the bound the cache holds one entry per
// distinct hash forever — the leak a long-lived `wal stream` CAS hit.
func TestCAS_MarkSeenBounded(t *testing.T) {
	const cap = 100
	c := &CAS{seenCap: cap}

	const inserts = 10_000
	for i := 0; i < inserts; i++ {
		c.markSeen(hashOfInt(i))
	}
	if n := seenLen(c); n > cap {
		t.Fatalf("seen cache holds %d entries after %d distinct inserts; must stay <= cap %d (it is cleared on overflow)", n, inserts, cap)
	}

	// unmarkSeen keeps the count in step so the bound keeps working.
	h := hashOfInt(1_000_000)
	c.markSeen(h)
	before := c.seenCount.Load()
	c.unmarkSeen(h)
	if got := c.seenCount.Load(); got != before-1 {
		t.Errorf("unmarkSeen: seenCount = %d, want %d", got, before-1)
	}
}

// TestCAS_SeenCacheDisabled: a zero/negative limit opts out of the bound
// (the cache grows unbounded — only safe for a short-lived CAS).
func TestCAS_MarkSeenUnboundedWhenDisabled(t *testing.T) {
	c := &CAS{seenCap: 0}
	const inserts = 5_000
	for i := 0; i < inserts; i++ {
		c.markSeen(hashOfInt(i))
	}
	if n := seenLen(c); n != inserts {
		t.Fatalf("with the bound disabled the cache should hold all %d entries; got %d", inserts, n)
	}
}

// TestCAS_DefaultSeenCapApplied: every CAS gets the default bound unless
// overridden, so the leak is fixed without per-call-site wiring.
func TestCAS_DefaultSeenCapApplied(t *testing.T) {
	sp := openTmpFS(t)
	if got := NewCAS(sp).seenCap; got != defaultSeenCacheLimit {
		t.Errorf("NewCAS seenCap = %d, want default %d", got, defaultSeenCacheLimit)
	}
	if got := NewCAS(sp, WithSeenCacheLimit(64)).seenCap; got != 64 {
		t.Errorf("WithSeenCacheLimit(64) seenCap = %d, want 64", got)
	}
	if got := NewCAS(sp, WithSeenCacheLimit(-1)).seenCap; got != 0 {
		t.Errorf("WithSeenCacheLimit(-1) seenCap = %d, want 0 (disabled)", got)
	}
}

// TestCAS_PutChunkSeenCacheBounded proves the bound holds end-to-end
// through the real PutChunk path (not just markSeen directly): writing
// many more distinct chunks than the cap leaves the cache bounded, while
// every chunk is still durably stored.
func TestCAS_PutChunkSeenCacheBounded(t *testing.T) {
	sp := openTmpFS(t)
	const cap = 64
	c := NewCAS(sp, WithSeenCacheLimit(cap))

	const chunks = 1000
	for i := 0; i < chunks; i++ {
		if _, err := c.PutChunk(context.Background(), []byte(fmt.Sprintf("chunk-%d", i))); err != nil {
			t.Fatalf("PutChunk %d: %v", i, err)
		}
	}
	if n := seenLen(c); n > cap {
		t.Fatalf("seen cache holds %d entries after %d distinct PutChunk calls; must stay <= cap %d", n, chunks, cap)
	}
	// Every chunk is still retrievable — dedup correctness is unaffected
	// by cache eviction (the IfNotExists Put is the backstop).
	for i := 0; i < chunks; i++ {
		h := Hash(sha256.Sum256([]byte(fmt.Sprintf("chunk-%d", i))))
		ok, err := c.HasChunk(context.Background(), h)
		if err != nil {
			t.Fatalf("HasChunk %d: %v", i, err)
		}
		if !ok {
			t.Fatalf("chunk %d missing after a bounded-cache run", i)
		}
	}
}

func openTmpFS(t *testing.T) storage.StoragePlugin {
	t.Helper()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: t.TempDir()}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	return sp
}
