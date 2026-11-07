package repo_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// slowGets delays every chunk Get and records how many overlap.
type slowGets struct {
	storage.StoragePlugin
	delay    time.Duration
	inFlight atomic.Int32
	peak     atomic.Int32
	gets     atomic.Int32
}

func (s *slowGets) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	n := s.inFlight.Add(1)
	defer s.inFlight.Add(-1)
	for {
		p := s.peak.Load()
		if n <= p || s.peak.CompareAndSwap(p, n) {
			break
		}
	}
	s.gets.Add(1)
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return s.StoragePlugin.Get(ctx, key)
}

func putChunks(t *testing.T, sp storage.StoragePlugin, n int) []repo.Hash {
	t.Helper()
	cas := repo.NewCAS(sp)
	out := make([]repo.Hash, n)
	for i := range out {
		ci, err := cas.PutChunk(context.Background(), []byte(fmt.Sprintf("chunk body %04d", i)))
		if err != nil {
			t.Fatal(err)
		}
		out[i] = ci.Hash
	}
	return out
}

// Issue #97: chunks were read one after the other. FetchOrdered reads
// ahead, and still hands the bodies over in the order asked for — a
// restore writes them to the file in that order.
func TestFetchOrdered_ReadsAheadDeliversInOrder(t *testing.T) {
	sp, _ := newGCRepo(t)
	hashes := putChunks(t, sp, 64)
	slow := &slowGets{StoragePlugin: sp, delay: 20 * time.Millisecond}
	cas := repo.NewCAS(slow, repo.WithFetchConcurrency(8))

	var got []repo.Hash
	err := cas.FetchOrdered(context.Background(), slices.Values(hashes), func(h repo.Hash, body []byte, err error) error {
		if err != nil {
			return err
		}
		if want := fmt.Sprintf("chunk body %04d", len(got)); string(body) != want {
			return fmt.Errorf("position %d carries %q, want %q", len(got), body, want)
		}
		got = append(got, h)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, hashes) {
		t.Fatal("chunks were delivered out of order")
	}
	if p := slow.peak.Load(); p < 2 || p > 8 {
		t.Errorf("peak concurrent reads = %d, want 2..8 (the budget)", p)
	}
}

// The budget belongs to the CAS, not to the call: sixteen files restored
// side by side must not each read ahead by the full amount.
func TestFetchOrdered_BudgetIsSharedAcrossCallers(t *testing.T) {
	sp, _ := newGCRepo(t)
	hashes := putChunks(t, sp, 96)
	slow := &slowGets{StoragePlugin: sp, delay: 10 * time.Millisecond}
	cas := repo.NewCAS(slow, repo.WithFetchConcurrency(4))

	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for w := 0; w < 6; w++ {
		part := hashes[w*16 : (w+1)*16]
		wg.Add(1)
		go func() {
			defer wg.Done()
			i := 0
			errs <- cas.FetchOrdered(context.Background(), slices.Values(part), func(h repo.Hash, _ []byte, err error) error {
				if err == nil && h != part[i] {
					err = fmt.Errorf("out of order at %d", i)
				}
				i++
				return err
			})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if p := slow.peak.Load(); p > 4 {
		t.Errorf("peak concurrent reads = %d across 6 callers, want at most the budget of 4", p)
	}
	if n := slow.gets.Load(); n != 96 {
		t.Errorf("%d reads for 96 chunks", n)
	}
}

// fn's error stops the walk, comes back unchanged, and nothing is left
// running; a failed read is fn's to judge (verify counts it and goes on).
func TestFetchOrdered_ErrorsAndCancellation(t *testing.T) {
	sp, _ := newGCRepo(t)
	hashes := putChunks(t, sp, 40)
	missing := repo.Hash{0xde, 0xad}
	withMissing := slices.Concat(hashes[:5], []repo.Hash{missing}, hashes[5:])
	cas := repo.NewCAS(sp, repo.WithFetchConcurrency(8))

	var failed, ok int
	err := cas.FetchOrdered(context.Background(), slices.Values(withMissing), func(h repo.Hash, _ []byte, err error) error {
		if err != nil {
			if h != missing || !errors.Is(err, storage.ErrNotFound) {
				return fmt.Errorf("unexpected failure for %s: %w", h, err)
			}
			failed++
			return nil
		}
		ok++
		return nil
	})
	if err != nil || failed != 1 || ok != 40 {
		t.Fatalf("err=%v failed=%d ok=%d; want nil/1/40", err, failed, ok)
	}

	stop := errors.New("stop here")
	calls := 0
	err = cas.FetchOrdered(context.Background(), slices.Values(hashes), func(repo.Hash, []byte, error) error {
		calls++
		if calls == 3 {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) || calls != 3 {
		t.Fatalf("err=%v after %d calls; want the callback's error after 3", err, calls)
	}

	ctx, cancel := context.WithCancel(context.Background())
	calls = 0
	err = cas.FetchOrdered(ctx, slices.Values(hashes), func(_ repo.Hash, _ []byte, err error) error {
		calls++
		if calls == 2 {
			cancel()
		}
		return err
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled walk returned %v", err)
	}
	if calls == len(hashes) {
		t.Error("a cancelled walk read every chunk")
	}

	// The budget is whole again after an aborted walk.
	n := 0
	if err := cas.FetchOrdered(context.Background(), slices.Values(hashes), func(_ repo.Hash, _ []byte, err error) error {
		n++
		return err
	}); err != nil || n != len(hashes) {
		t.Fatalf("walk after an aborted one: err=%v n=%d", err, n)
	}
}

func TestClampFetchConcurrency(t *testing.T) {
	for in, want := range map[int]int{-1: 16, 0: 16, 1: 1, 16: 16, 64: 64, 65: 64} {
		if got := repo.ClampFetchConcurrency(in); got != want {
			t.Errorf("ClampFetchConcurrency(%d) = %d, want %d", in, got, want)
		}
	}
}

// breakingBody fails mid-read for the first `breaks` opens of each key.
type breakingBody struct {
	storage.StoragePlugin
	mu     sync.Mutex
	breaks int
	opens  map[string]int
}

func (b *breakingBody) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	rc, err := b.StoragePlugin.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.opens[key]++
	n := b.opens[key]
	b.mu.Unlock()
	if n <= b.breaks {
		return brokenAfter{rc, 3}, nil
	}
	return rc, nil
}

type brokenAfter struct {
	io.ReadCloser
	n int
}

func (b brokenAfter) Read(p []byte) (int, error) {
	if len(p) > b.n {
		p = p[:b.n]
	}
	n, _ := b.ReadCloser.Read(p)
	return n, errors.New("read tcp 10.0.0.4:51234->20.60.1.1:443: read: connection reset by peer")
}

// Issue #97: a chunk body that breaks off mid-read is fetched again; a
// body that keeps breaking still fails, and a missing chunk is not
// retried at all.
func TestGetChunkBytes_RetriesABrokenBody(t *testing.T) {
	repo.SetChunkReadBackoffForTest(t, func(int) time.Duration { return 0 })
	ctx := context.Background()
	sp, _ := newGCRepo(t)
	ci, err := repo.NewCAS(sp).PutChunk(ctx, []byte("a chunk worth reading twice"))
	if err != nil {
		t.Fatal(err)
	}
	key := repo.ChunkKey(ci.Hash)

	flaky := &breakingBody{StoragePlugin: sp, breaks: 4, opens: map[string]int{}}
	body, err := repo.NewCAS(flaky).GetChunkBytes(ctx, ci.Hash)
	if err != nil || string(body) != "a chunk worth reading twice" {
		t.Fatalf("body=%q err=%v; four broken reads must be retried", body, err)
	}
	if flaky.opens[key] != 5 {
		t.Errorf("%d opens, want 5", flaky.opens[key])
	}

	dead := &breakingBody{StoragePlugin: sp, breaks: 99, opens: map[string]int{}}
	if _, err := repo.NewCAS(dead).GetChunkBytes(ctx, ci.Hash); err == nil {
		t.Fatal("a body that never completes was reported as read")
	}
	if dead.opens[key] != 5 {
		t.Errorf("%d opens of a persistently broken body, want 5 (one read, four retries)", dead.opens[key])
	}

	counting := &slowGets{StoragePlugin: sp}
	if _, err := repo.NewCAS(counting).GetChunkBytes(ctx, repo.Hash{1, 2, 3}); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("missing chunk: %v", err)
	}
	if n := counting.gets.Load(); n != 1 {
		t.Errorf("a missing chunk was opened %d times, want 1", n)
	}
}
