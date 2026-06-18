package repo_test

import (
	"context"
	"crypto/sha256"
	"io"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// openFS returns a fresh fs-backed StoragePlugin over a temp dir.
func openFS(t *testing.T) *fs.Plugin {
	t.Helper()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: t.TempDir()}}); err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	return sp
}

// countingStorage wraps a StoragePlugin and tallies Put / Stat calls,
// so a test can PROVE the dedup-hint path skipped the Put rather than
// reaching it and getting ErrAlreadyExists — the two end states are
// otherwise indistinguishable from the ChunkInfo alone.
type countingStorage struct {
	storage.StoragePlugin
	puts  atomic.Int64
	stats atomic.Int64
}

func (c *countingStorage) Put(ctx context.Context, key string, r io.Reader, opts storage.PutOptions) (storage.PutResult, error) {
	c.puts.Add(1)
	return c.StoragePlugin.Put(ctx, key, r, opts)
}

func (c *countingStorage) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	c.stats.Add(1)
	return c.StoragePlugin.Stat(ctx, key)
}

func TestDedupStats_Counts(t *testing.T) {
	c := repo.NewCAS(openFS(t))
	ctx := context.Background()

	if _, err := c.PutChunk(ctx, []byte("alpha")); err != nil { // miss
		t.Fatal(err)
	}
	if _, err := c.PutChunk(ctx, []byte("alpha")); err != nil { // in-memory hit
		t.Fatal(err)
	}
	if _, err := c.PutChunk(ctx, []byte("beta")); err != nil { // miss
		t.Fatal(err)
	}

	got := c.DedupStats()
	want := repo.DedupStats{Misses: 2, HitsInMemory: 1, HitsStorage: 0}
	if got != want {
		t.Fatalf("DedupStats = %+v, want %+v", got, want)
	}
	if got.Total() != 3 {
		t.Errorf("Total = %d, want 3", got.Total())
	}
	if hr := got.HitRate(); hr < 0.333 || hr > 0.334 {
		t.Errorf("HitRate = %v, want ~0.333", hr)
	}
	if (repo.DedupStats{}).HitRate() != 0 {
		t.Error("HitRate of an empty stats must be 0, not NaN")
	}
}

// A hinted chunk that really is in the repo must be confirmed by a
// single Stat probe — and NOT reach Put at all (no wasted compress /
// encrypt / upload).
func TestDedupHints_ConfirmedHitSkipsPut(t *testing.T) {
	ctx := context.Background()
	sp := openFS(t)
	body := []byte("a chunk that already lives in the repo")

	// A prior backup wrote this chunk.
	if _, err := repo.NewCAS(sp).PutChunk(ctx, body); err != nil {
		t.Fatalf("seed put: %v", err)
	}

	// A new run (fresh seen-map) re-puts it, with a hint.
	counting := &countingStorage{StoragePlugin: sp}
	hints := map[repo.Hash]struct{}{repo.Hash(sha256.Sum256(body)): {}}
	c := repo.NewCAS(counting, repo.WithDedupHints(hints))

	info, err := c.PutChunk(ctx, body)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if !info.Deduped {
		t.Error("hinted, already-present chunk must report Deduped")
	}
	if got := counting.stats.Load(); got != 1 {
		t.Errorf("Stat calls = %d, want exactly 1 (the hint probe)", got)
	}
	if got := counting.puts.Load(); got != 0 {
		t.Errorf("Put calls = %d, want 0 — the confirmed hit must skip the write", got)
	}
	if s := c.DedupStats(); s.HitsStorage != 1 || s.Misses != 0 {
		t.Errorf("DedupStats = %+v, want HitsStorage=1 Misses=0", s)
	}
}

// A stale hint (hash hinted but the chunk is NOT actually in the repo)
// must fall through to the normal write path — correctness over the
// optimization.
func TestDedupHints_StaleHintFallsThrough(t *testing.T) {
	ctx := context.Background()
	body := []byte("hinted but never actually written")

	counting := &countingStorage{StoragePlugin: openFS(t)}
	hints := map[repo.Hash]struct{}{repo.Hash(sha256.Sum256(body)): {}}
	c := repo.NewCAS(counting, repo.WithDedupHints(hints))

	info, err := c.PutChunk(ctx, body)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if info.Deduped {
		t.Error("a stale hint must not make a never-written chunk look deduped")
	}
	if got := counting.stats.Load(); got != 1 {
		t.Errorf("Stat calls = %d, want 1 (the probe that missed)", got)
	}
	if got := counting.puts.Load(); got != 1 {
		t.Errorf("Put calls = %d, want 1 — must fall through and write", got)
	}
	if s := c.DedupStats(); s.Misses != 1 || s.HitsStorage != 0 {
		t.Errorf("DedupStats = %+v, want Misses=1 HitsStorage=0", s)
	}

	// The chunk really did land — a follow-up read returns it.
	if got, err := c.GetChunkBytes(ctx, info.Hash); err != nil || string(got) != string(body) {
		t.Errorf("GetChunkBytes after fall-through = (%q, %v), want the body", got, err)
	}
}

// With no hints, PutChunk must never issue a Stat probe — the feature
// is strictly opt-in and adds zero overhead to a first backup.
func TestDedupHints_NilIsZeroOverhead(t *testing.T) {
	ctx := context.Background()
	counting := &countingStorage{StoragePlugin: openFS(t)}
	c := repo.NewCAS(counting) // no WithDedupHints

	for i := 0; i < 3; i++ {
		if _, err := c.PutChunk(ctx, []byte("same body every time")); err != nil {
			t.Fatal(err)
		}
	}
	if got := counting.stats.Load(); got != 0 {
		t.Errorf("Stat calls = %d, want 0 — no hints means no probe", got)
	}
	if s := c.DedupStats(); s.Misses != 1 || s.HitsInMemory != 2 {
		t.Errorf("DedupStats = %+v, want Misses=1 HitsInMemory=2", s)
	}
}
