package repo

import (
	"context"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (f *fakeClock) now() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.t }
func (f *fakeClock) advance(d time.Duration) {
	f.mu.Lock()
	f.t = f.t.Add(d)
	f.mu.Unlock()
}

func newTrustCAS(t *testing.T, opts ...CASOption) (*CAS, storage.StoragePlugin, *fakeClock) {
	t.Helper()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: t.TempDir()}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	clk := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	c := NewCAS(sp, opts...)
	c.now = clk.now
	return c, sp, clk
}

// C5: the long-lived `wal stream` CAS. A chunk written days ago stays in
// the positive cache after the segments that referenced it were pruned
// and gc swept it. A later segment with the same content must NOT be
// vouched for by that memory: the hit has to land in the adopted set so
// the commit-time gate Stats it, finds it gone, and — after the gate
// forgets it — the retry rewrites it.
func TestCAS_StaleSeenHitIsAdoptedNotTrusted(t *testing.T) {
	ctx := context.Background()
	c, sp, clk := newTrustCAS(t)
	body := []byte("full-page image that recurs in WAL")

	ci, err := c.PutChunk(ctx, body)
	if err != nil || ci.Deduped {
		t.Fatalf("first put: %+v %v", ci, err)
	}
	// Segment published; the stream releases its adoptions as walsink does.
	c.ForgetAdopted(ci.Hash)

	// Days later: every segment referencing it has been pruned and gc
	// swept the orphan out of the repository.
	clk.advance(72 * time.Hour)
	if err := sp.Delete(ctx, ChunkKey(ci.Hash)); err != nil {
		t.Fatal(err)
	}

	again, err := c.PutChunk(ctx, body)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Deduped {
		t.Fatal("expected an in-memory dedup hit")
	}
	if !c.WasAdopted(ci.Hash) {
		t.Fatal("a cache hit on a chunk written 72h ago returned Deduped without adoption: " +
			"no publish-time gate will Stat it, and the segment is published over a deleted chunk")
	}

	// The gate Stats it, finds it gone, and forgets it...
	missing, unchecked, err := c.VerifyAdopted(ctx, []Hash{ci.Hash})
	if err != nil || unchecked != 0 || len(missing) != 1 || missing[0] != ci.Hash {
		t.Fatalf("VerifyAdopted = %v, %d, %v; want the deleted chunk reported missing", missing, unchecked, err)
	}
	// ...so the retry writes it fresh instead of adopting the memory again.
	retry, err := c.PutChunk(ctx, body)
	if err != nil {
		t.Fatal(err)
	}
	if retry.Deduped || c.WasAdopted(ci.Hash) {
		t.Fatalf("retry after a failed gate must rewrite the chunk; got Deduped=%v adopted=%v", retry.Deduped, c.WasAdopted(ci.Hash))
	}
	if _, err := sp.Stat(ctx, ChunkKey(ci.Hash)); err != nil {
		t.Fatalf("chunk not back in the repository after the retry: %v", err)
	}
}

// Our own fresh write is protected by gc's --min-chunk-age floor (young
// mtime), so a hit inside seenWriteTrust stays cheap: no adoption, no
// publish-time Stat. The window closes at seenWriteTrust.
func TestCAS_FreshOwnWriteHitIsTrustedUntilWindowCloses(t *testing.T) {
	ctx := context.Background()
	c, _, clk := newTrustCAS(t)
	ci, err := c.PutChunk(ctx, []byte("fresh"))
	if err != nil {
		t.Fatal(err)
	}
	clk.advance(seenWriteTrust - time.Second)
	if _, err := c.PutChunk(ctx, []byte("fresh")); err != nil {
		t.Fatal(err)
	}
	if c.WasAdopted(ci.Hash) {
		t.Fatal("a hit inside the trust window of our own write was marked adopted")
	}
	clk.advance(time.Second)
	if _, err := c.PutChunk(ctx, []byte("fresh")); err != nil {
		t.Fatal(err)
	}
	if !c.WasAdopted(ci.Hash) {
		t.Fatal("a hit at the end of the trust window was not marked adopted")
	}
}

// An entry established by VERIFICATION — a lost IfNotExists race here —
// was never protected by an mtime of ours, so it gets no window at all:
// once the publishing writer releases the adoption, the very next hit
// must be adopted again.
func TestCAS_VerifiedEntryHitIsAlwaysAdopted(t *testing.T) {
	ctx := context.Background()
	c, sp, _ := newTrustCAS(t)
	other := NewCAS(sp)
	ci, err := other.PutChunk(ctx, []byte("written by someone else"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.PutChunk(ctx, []byte("written by someone else"))
	if err != nil || !got.Deduped || !c.WasAdopted(ci.Hash) {
		t.Fatalf("lost race: %+v %v adopted=%v", got, err, c.WasAdopted(ci.Hash))
	}
	c.ForgetAdopted(ci.Hash)
	if _, err := c.PutChunk(ctx, []byte("written by someone else")); err != nil {
		t.Fatal(err)
	}
	if !c.WasAdopted(ci.Hash) {
		t.Fatal("a cache hit on a verified (not written) entry was trusted without adoption")
	}
}
