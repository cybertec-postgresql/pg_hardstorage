package spill

import (
	"bytes"
	"container/heap"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"
)

func key(i int) Key {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(i))
	return sha256.Sum256(b[:])
}

func TestHashSet_MatchesMapAcrossSpills(t *testing.T) {
	s := NewHashSet(Options{MemEntries: 100, Dir: t.TempDir()})
	defer s.Close()
	want := map[Key]struct{}{}
	for i := 0; i < 5000; i++ {
		k := key(i % 3700) // duplicates across flush boundaries
		_, had := want[k]
		if got := s.Add(k); got == had {
			t.Fatalf("Add(%d) new=%v but model had=%v", i, got, had)
		}
		want[k] = struct{}{}
	}
	if s.Spilled() == 0 {
		t.Fatal("expected the set to have spilled")
	}
	for i := 0; i < 8000; i++ {
		_, w := want[key(i)]
		if s.Has(key(i)) != w {
			t.Fatalf("Has(%d) = %v, want %v", i, !w, w)
		}
	}
	if n := s.Len(); n != len(want) {
		t.Fatalf("Len = %d, want %d", n, len(want))
	}
	if s.Spilled() != 1 {
		t.Fatalf("Len should leave one merged run, have %d", s.Spilled())
	}
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestHashSet_ForEachSortedUnique(t *testing.T) {
	s := NewHashSet(Options{MemEntries: 64, Dir: t.TempDir()})
	defer s.Close()
	want := map[Key]struct{}{}
	for i := 0; i < 1000; i++ {
		s.Add(key(i % 777))
		want[key(i%777)] = struct{}{}
	}
	var got []Key
	if err := s.ForEach(func(k Key) error { got = append(got, k); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d keys, want %d", len(got), len(want))
	}
	if !sort.SliceIsSorted(got, func(i, j int) bool { return bytes.Compare(got[i][:], got[j][:]) < 0 }) {
		t.Fatal("ForEach not ascending")
	}
}

func TestHashSet_RunCountStaysLogarithmic(t *testing.T) {
	s := NewHashSet(Options{MemEntries: 16, Dir: t.TempDir()})
	defer s.Close()
	for i := 0; i < 16*200; i++ {
		s.Add(key(i))
	}
	if n := s.Spilled(); n > 8 {
		t.Fatalf("200 flushes left %d runs; want <= log2(200)+1", n)
	}
}

func TestHashSet_InMemoryNeverTouchesDisk(t *testing.T) {
	dir := t.TempDir()
	s := NewHashSet(Options{MemEntries: 1000, Dir: dir})
	for i := 0; i < 500; i++ {
		s.Add(key(i))
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatalf("small set created %d spill entries", len(ents))
	}
	s.Close()
}

func TestHashSet_CloseRemovesSpill(t *testing.T) {
	dir := t.TempDir()
	s := NewHashSet(Options{MemEntries: 10, Dir: dir})
	for i := 0; i < 100; i++ {
		s.Add(key(i))
	}
	if ents, _ := os.ReadDir(dir); len(ents) == 0 {
		t.Fatal("expected spill dir")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatalf("Close left %d entries", len(ents))
	}
}

func TestHashSet_Concurrent(t *testing.T) {
	s := NewHashSet(Options{MemEntries: 200, Dir: t.TempDir()})
	defer s.Close()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				s.Add(key(i))
				_ = s.Has(key(i + g))
			}
		}(g)
	}
	wg.Wait()
	if n := s.Len(); n != 2000 {
		t.Fatalf("Len = %d, want 2000", n)
	}
}

func TestBloom_NoFalseNegatives(t *testing.T) {
	b := NewBloom(10000)
	for i := 0; i < 10000; i++ {
		b.Add(key(i))
	}
	for i := 0; i < 10000; i++ {
		if !b.Test(key(i)) {
			t.Fatalf("false negative at %d", i)
		}
	}
	fp := 0
	for i := 10000; i < 30000; i++ {
		if b.Test(key(i)) {
			fp++
		}
	}
	if fp > 600 { // ~1% expected of 20000 = 200; allow 3%
		t.Fatalf("false positives %d/20000 too high", fp)
	}
}

// A run file truncated behind the set's back is damage, not absence: Has
// must answer "present" (a GC then keeps the chunk) and Err must say why.
func TestHashSet_TruncatedRunFailsClosed(t *testing.T) {
	s := NewHashSet(Options{Dir: t.TempDir(), MemEntries: 1000})
	defer s.Close()
	const n = 5000
	for i := 0; i < n; i++ {
		s.Add(key(i))
	}
	s.mu.Lock()
	paths := make([]string, 0, len(s.runs))
	for _, r := range s.runs {
		paths = append(paths, r.path)
	}
	s.mu.Unlock()
	if len(paths) == 0 {
		t.Fatal("fixture: nothing spilled")
	}
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(p, fi.Size()/2); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < n; i++ {
		if !s.Has(key(i)) {
			t.Fatalf("member %d reported absent after its run file was truncated", i)
		}
	}
	if s.Err() == nil {
		t.Fatal("Err() is nil although a run file could not be read")
	}
}

// A failed flush (disk full) must not make every later Add re-sort the
// whole heap buffer: the set keeps accepting members, reports the error,
// and retries only as the buffer doubles.
func TestHashSet_FailedFlushDoesNotRetryEveryAdd(t *testing.T) {
	s := NewHashSet(Options{Dir: t.TempDir(), MemEntries: 1000})
	defer s.Close()
	for i := 0; i < 1000; i++ { // first flush succeeds and creates the spill dir
		s.Add(key(i))
	}
	s.mu.Lock()
	dir := s.dir
	s.mu.Unlock()
	if dir == "" {
		t.Fatal("fixture: nothing spilled")
	}
	// A read-only spill dir makes every later run creation fail, after the
	// heap buffer has been sorted: the cost the retry-every-Add paid.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	if f, err := os.OpenFile(filepath.Join(dir, "probe"), os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		f.Close()
		t.Skip("directory permissions are not enforced (running as root?)")
	}
	start := time.Now()
	const n = 200_000
	for i := 1000; i < n; i++ {
		if !s.Add(key(i)) {
			t.Fatalf("Add(%d) reported a duplicate", i)
		}
	}
	if s.Err() == nil {
		t.Fatal("Err() is nil after failed flushes")
	}
	// Retrying on every Add sorts up to 200k keys 199k times (minutes).
	if d := time.Since(start); d > 20*time.Second {
		t.Fatalf("%d Adds after a failed flush took %v", n, d)
	}
}

// mergeHeap must satisfy container/heap's full contract.
func TestMergeHeap_PushPop(t *testing.T) {
	var h mergeHeap
	heap.Push(&h, &mergeSrc{cur: key(2)})
	heap.Push(&h, &mergeSrc{cur: key(1)})
	heap.Push(&h, &mergeSrc{cur: key(3)})
	if h.Len() != 3 {
		t.Fatalf("Len = %d", h.Len())
	}
	prev := heap.Pop(&h).(*mergeSrc).cur
	for h.Len() > 0 {
		cur := heap.Pop(&h).(*mergeSrc).cur
		if bytes.Compare(prev[:], cur[:]) > 0 {
			t.Fatal("heap popped out of order")
		}
		prev = cur
	}
}

// The Bloom filter heap is capped; a set far past the cap stays exact (a
// leaky filter only costs extra block reads).
func TestHashSet_BloomCapKeepsSetExact(t *testing.T) {
	if b := newBloom(1<<40, 1<<20); len(b.bits)*8 > 1<<20 {
		t.Fatalf("bloom is %d bytes, cap is %d", len(b.bits)*8, 1<<20)
	}
	if b := newBloom(1000, 1<<20); len(b.bits)*8 > 2000 {
		t.Fatalf("a small set must not be inflated: %d bytes", len(b.bits)*8)
	}
	s := NewHashSet(Options{MemEntries: 64, Dir: t.TempDir(), MaxBloomBytes: 64})
	defer s.Close()
	const n = 2000
	for i := 0; i < n; i++ {
		s.Add(sha256.Sum256([]byte(fmt.Sprintf("k%d", i))))
	}
	if s.Spilled() == 0 {
		t.Fatal("expected the set to spill")
	}
	for i := 0; i < n; i++ {
		if !s.Has(sha256.Sum256([]byte(fmt.Sprintf("k%d", i)))) {
			t.Fatalf("lost key %d", i)
		}
	}
	for i := 0; i < n; i++ {
		if s.Has(sha256.Sum256([]byte(fmt.Sprintf("absent%d", i)))) {
			t.Fatalf("false positive for absent key %d", i)
		}
	}
	if got := s.Len(); got != n {
		t.Fatalf("Len = %d, want %d", got, n)
	}
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}
}
