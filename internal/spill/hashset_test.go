package spill

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"os"
	"sort"
	"sync"
	"testing"
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
