package spill

import (
	"errors"
	"os"
	"strconv"
	"sync"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/leakcheck"
)

func scaleN(small, big int) int {
	if os.Getenv("PGHS_SCALE") == "1" {
		return big
	}
	return small
}

// Repeated create/spill/merge/query/Close cycles must return every fd,
// leave no spill directory behind, and keep the live heap flat.
func TestHashSet_LifecycleDoesNotLeak(t *testing.T) {
	dir := t.TempDir()
	base := leakcheck.Take()
	cycles := scaleN(300, 3000)
	leakcheck.Plateau(t, cycles, cycles/16, 8<<20, func(i int) {
		s := NewHashSet(Options{MemEntries: 64, Dir: dir})
		for j := 0; j < 700; j++ {
			s.Add(key(i*7 + j))
		}
		if s.Spilled() == 0 {
			t.Fatal("expected a spill")
		}
		for j := 0; j < 50; j++ {
			s.Has(key(j))
		}
		switch i % 4 {
		case 0:
			_ = s.Len() // compactAll
		case 1:
			_ = s.ForEach(func(Key) error { return nil })
		case 2:
			// early ForEach abort
			_ = s.ForEach(func(Key) error { return errors.New("stop") })
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil { // idempotent
			t.Fatal(err)
		}
	})
	if left := leakcheck.DirEntries(dir); len(left) != 0 {
		t.Fatalf("spill dirs left behind: %v", left)
	}
	leakcheck.AssertNoLeak(t, base, leakcheck.Tolerance{})
}

// A set that is abandoned mid-failure (flush error) must still release
// everything at Close.
func TestHashSet_FailedFlushThenCloseDoesNotLeak(t *testing.T) {
	dir := t.TempDir()
	base := leakcheck.Take()
	old := freeBytesFn
	defer func() { freeBytesFn = old }()
	for i := 0; i < 100; i++ {
		s := NewHashSet(Options{MemEntries: 16, Dir: dir})
		for j := 0; j < 40; j++ {
			s.Add(key(j + i*100))
		}
		freeBytesFn = func(string) (int64, bool) { return 0, true } // ENOSPC preflight
		for j := 0; j < 200; j++ {
			s.Add(key(10_000 + j + i*1000))
		}
		if s.Err() == nil {
			t.Fatal("expected sticky error")
		}
		freeBytesFn = old
		_ = s.Close()
	}
	if left := leakcheck.DirEntries(dir); len(left) != 0 {
		t.Fatalf("spill dirs left behind: %v", left)
	}
	leakcheck.AssertNoLeak(t, base, leakcheck.Tolerance{})
}

// Many concurrent sets, each hammered by several goroutines, then closed.
func TestHashSet_ConcurrentLifecycleDoesNotLeak(t *testing.T) {
	dir := t.TempDir()
	base := leakcheck.Take()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < scaleN(40, 400); i++ {
				s := NewHashSet(Options{MemEntries: 50, Dir: dir})
				var iw sync.WaitGroup
				for w := 0; w < 4; w++ {
					iw.Add(1)
					go func(w int) {
						defer iw.Done()
						for j := 0; j < 300; j++ {
							s.Add(key(g*1_000_000 + w*1000 + j))
							s.Has(key(j))
						}
					}(w)
				}
				iw.Wait()
				_ = s.Close()
			}
		}(g)
	}
	wg.Wait()
	if left := leakcheck.DirEntries(dir); len(left) != 0 {
		t.Fatalf("spill dirs left: %d", len(left))
	}
	leakcheck.AssertNoLeak(t, base, leakcheck.Tolerance{})
}

// One long-lived set absorbing a stream much larger than MemEntries: the
// heap must stay bounded (runs merged LSM-style, blooms capped) and the
// number of open fds equals the number of live runs (logarithmic).
func TestHashSet_LongLivedBoundedGrowth(t *testing.T) {
	dir := t.TempDir()
	n := scaleN(400_000, 40_000_000)
	mem := 20_000
	s := NewHashSet(Options{MemEntries: mem, Dir: dir})
	defer s.Close()
	base := leakcheck.Take()
	maxRuns := 0
	step := n / 40
	var warm uint64
	for i := 0; i < n; i++ {
		s.Add(key(i))
		if (i+1)%step == 0 {
			if r := s.Spilled(); r > maxRuns {
				maxRuns = r
			}
			h := leakcheck.LiveHeap()
			t.Logf("i=%d runs=%d heap=%dMiB", i+1, s.Spilled(), h>>20)
			if i+1 == step*10 {
				warm = h
			}
			// heap budget: mem map + blooms (1.25B/key) + fences; allow 300B/mem-entry
			// plus 2 B per key stored.
			if warm != 0 && h > warm+uint64(i+1)*3/2+(64<<20) {
				t.Fatalf("heap grew %d MiB from warm-up %d MiB at %d keys", h>>20, warm>>20, i+1)
			}
		}
	}
	// logarithmic run count
	bound := 2
	for x := n / mem; x > 0; x >>= 1 {
		bound++
	}
	if maxRuns > bound {
		t.Fatalf("run count %d exceeds log bound %d (fds scale with runs)", maxRuns, bound)
	}
	cur := leakcheck.Take()
	if cur.FDs > base.FDs+bound+2 {
		t.Fatalf("fds %d vs base %d with %d runs", cur.FDs, base.FDs, s.Spilled())
	}
	if got := s.Len(); got != n {
		t.Fatalf("len %d want %d", got, n)
	}
	_ = strconv.Itoa
}
