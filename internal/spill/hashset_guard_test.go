package spill

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// A key Add already found in a run is answered from the hot cache next
// time: even with the run file truncated behind the set's back, re-adding
// it neither reads the file nor trips the sticky error.
func TestHashSet_ReAddOfSpilledKeyUsesHotCache(t *testing.T) {
	s := NewHashSet(Options{MemEntries: 8, Dir: t.TempDir()})
	defer s.Close()
	for i := 0; i < 40; i++ {
		s.Add(key(i))
	}
	if s.Spilled() == 0 {
		t.Fatal("expected a spill")
	}
	if s.Add(key(3)) { // first re-add: found in a run, now cached
		t.Fatal("key 3 reported absent")
	}
	for _, r := range s.runs {
		if err := os.Truncate(r.path, 0); err != nil {
			t.Fatal(err)
		}
	}
	if s.Add(key(3)) || !s.Has(key(3)) {
		t.Fatal("cached key should still be present")
	}
	if err := s.Err(); err != nil {
		t.Fatalf("hot cache should have avoided the run read: %v", err)
	}
	if s.Add(key(4)) { // uncached: must hit the damaged run and fail closed
		t.Fatal("damaged run must answer present")
	}
	if s.Err() == nil {
		t.Fatal("damaged run not recorded")
	}
}

func TestHashSet_PreflightRefusesWhenSpillVolumeTooSmall(t *testing.T) {
	saved := freeBytesFn
	freeBytesFn = func(string) (int64, bool) { return 64, true }
	t.Cleanup(func() { freeBytesFn = saved })
	s := NewHashSet(Options{MemEntries: 4, Dir: t.TempDir()})
	defer s.Close()
	for i := 0; i < 10; i++ {
		s.Add(key(i))
	}
	err := s.Err()
	if !errors.Is(err, ErrNoSpace) || !strings.Contains(err.Error(), "TMPDIR") {
		t.Fatalf("Err = %v, want ErrNoSpace naming TMPDIR", err)
	}
	if s.Spilled() != 0 {
		t.Fatal("nothing should have been written")
	}
}

func TestHashSet_CompactionPreflight(t *testing.T) {
	s := NewHashSet(Options{MemEntries: 4, Dir: t.TempDir()})
	defer s.Close()
	for i := 0; i < 30; i++ {
		s.Add(key(i))
	}
	if s.Spilled() < 2 {
		t.Skip("need several runs")
	}
	saved := freeBytesFn
	freeBytesFn = func(string) (int64, bool) { return 1, true }
	t.Cleanup(func() { freeBytesFn = saved })
	s.Len()
	if !errors.Is(s.Err(), ErrNoSpace) {
		t.Fatalf("Len compaction did not preflight: %v", s.Err())
	}
}

func TestHashSet_WarnsOnceWhenSpillDirIsTmpfs(t *testing.T) {
	savedFS, savedW := inMemoryFSFn, OnWarning
	inMemoryFSFn = func(string) bool { return true }
	var got []string
	OnWarning = func(m string) { got = append(got, m) }
	t.Cleanup(func() { inMemoryFSFn, OnWarning = savedFS, savedW })
	dir := t.TempDir()
	for n := 0; n < 2; n++ {
		s := NewHashSet(Options{MemEntries: 2, Dir: dir})
		for i := 0; i < 6; i++ {
			s.Add(key(i))
		}
		s.Close()
	}
	if len(got) != 1 || !strings.Contains(got[0], "tmpfs") {
		t.Fatalf("warnings = %q, want exactly one tmpfs warning", got)
	}
}
