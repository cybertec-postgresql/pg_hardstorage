package spill

import "testing"

// The spilled set must behave exactly like a map under any sequence of
// adds and lookups, whatever the flush size.
func FuzzHashSet_MatchesMap(f *testing.F) {
	f.Add([]byte{1, 2, 3, 1, 2, 3, 9}, uint8(2))
	f.Add([]byte{}, uint8(1))
	f.Fuzz(func(t *testing.T, ops []byte, mem uint8) {
		s := NewHashSet(Options{MemEntries: int(mem%16) + 1, Dir: t.TempDir()})
		defer s.Close()
		model := map[Key]struct{}{}
		for i, b := range ops {
			k := key(int(b) % 40)
			if i%3 == 2 { // lookup
				_, want := model[k]
				if s.Has(k) != want {
					t.Fatalf("Has(%d) wrong at op %d", b%40, i)
				}
				continue
			}
			_, had := model[k]
			if s.Add(k) == had {
				t.Fatalf("Add(%d) new=%v but model had=%v", b%40, !had, had)
			}
			model[k] = struct{}{}
		}
		if n := s.Len(); n != len(model) {
			t.Fatalf("Len = %d, want %d", n, len(model))
		}
		seen := 0
		if err := s.ForEach(func(Key) error { seen++; return nil }); err != nil || seen != len(model) {
			t.Fatalf("ForEach saw %d (err %v), want %d", seen, err, len(model))
		}
	})
}
