package repo_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// retentionRecorder advertises WORM and records every SetRetention, so a
// test can assert which adopted chunks had their lock extended.
type retentionRecorder struct {
	storage.StoragePlugin
	mu  sync.Mutex
	set map[string][]time.Time
}

func (r *retentionRecorder) Capabilities() storage.Capabilities {
	c := r.StoragePlugin.Capabilities()
	c.WORM = true
	return c
}

func (r *retentionRecorder) SetRetention(_ context.Context, key string, until time.Time, _ storage.WORMMode) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.set == nil {
		r.set = map[string][]time.Time{}
	}
	r.set[key] = append(r.set[key], until)
	return nil
}

func (r *retentionRecorder) calls(key string) []time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Time(nil), r.set[key]...)
}

// H44: a dedup hit adopts a chunk without writing it, so nothing touched
// its WORM lock. On a WORM repository a new long-retention backup must
// extend the adopted chunk's lock to its own deadline — on every
// adoption door (lost IfNotExists race, hint-confirmed Stat).
func TestCAS_AdoptionExtendsWORMRetention(t *testing.T) {
	ctx := context.Background()
	sp, _ := newGCRepo(t)
	rec := &retentionRecorder{StoragePlugin: sp}

	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	longer := old.Add(7 * 365 * 24 * time.Hour)

	// The first backup wrote the chunks under a short lock.
	first := repo.NewCAS(rec, repo.WithRetention(repo.CASRetention{RetainUntil: old, Mode: storage.WORMCompliance}))
	a, err := first.PutChunk(ctx, []byte("adopted via lost race"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := first.PutChunk(ctx, []byte("adopted via hint"))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(rec.calls(repo.ChunkKey(a.Hash))); n != 0 {
		t.Fatalf("a freshly written chunk is locked at Put time; got %d SetRetention calls", n)
	}

	// A seven-year backup deduplicates against both.
	second := repo.NewCAS(rec,
		repo.WithRetention(repo.CASRetention{RetainUntil: longer, Mode: storage.WORMCompliance}),
		repo.WithDedupHints(map[repo.Hash]struct{}{b.Hash: {}}))
	for _, body := range [][]byte{[]byte("adopted via lost race"), []byte("adopted via hint")} {
		ci, err := second.PutChunk(ctx, body)
		if err != nil {
			t.Fatal(err)
		}
		if !ci.Deduped {
			t.Fatalf("expected a dedup hit for %q", body)
		}
	}
	for _, h := range []repo.Hash{a.Hash, b.Hash} {
		got := rec.calls(repo.ChunkKey(h))
		if len(got) != 1 || !got[0].Equal(longer) {
			t.Errorf("chunk %s: SetRetention calls = %v, want exactly one extending to %v — "+
				"the new backup references a chunk whose lock expires first", h, got, longer)
		}
	}

	// A later in-memory hit in the same backup needs no second call: the
	// lock already covers the (fixed) deadline.
	if _, err := second.PutChunk(ctx, []byte("adopted via hint")); err != nil {
		t.Fatal(err)
	}
	if got := rec.calls(repo.ChunkKey(b.Hash)); len(got) != 1 {
		t.Errorf("repeat hit re-extended the lock: %v", got)
	}
}

// Under a moving deadline (the `wal stream` RetainUntilFunc), an
// in-memory hit on a chunk whose lock the deadline has overtaken must
// extend it again — with headroom so a hot chunk is not re-locked on
// every segment.
func TestCAS_MovingDeadlineReExtendsOnSeenHit(t *testing.T) {
	ctx := context.Background()
	sp, _ := newGCRepo(t)
	rec := &retentionRecorder{StoragePlugin: sp}

	var mu sync.Mutex
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	deadline := func() time.Time { mu.Lock(); defer mu.Unlock(); return now.Add(24 * time.Hour) }
	c := repo.NewCAS(rec, repo.WithRetention(repo.CASRetention{RetainUntilFunc: deadline, Mode: storage.WORMCompliance}))

	ci, err := c.PutChunk(ctx, []byte("hot wal chunk"))
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	now = now.Add(10 * time.Minute)
	mu.Unlock()
	if _, err := c.PutChunk(ctx, []byte("hot wal chunk")); err != nil {
		t.Fatal(err)
	}
	got := rec.calls(repo.ChunkKey(ci.Hash))
	if len(got) != 1 || got[0].Before(deadline()) {
		t.Fatalf("SetRetention calls = %v; want one covering %v", got, deadline())
	}
	// Ten more minutes: still inside the headroom, no new call.
	mu.Lock()
	now = now.Add(10 * time.Minute)
	mu.Unlock()
	if _, err := c.PutChunk(ctx, []byte("hot wal chunk")); err != nil {
		t.Fatal(err)
	}
	if got := rec.calls(repo.ChunkKey(ci.Hash)); len(got) != 1 {
		t.Errorf("re-extended inside the headroom: %v", got)
	}
}
