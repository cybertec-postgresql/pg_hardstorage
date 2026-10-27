package repo

import (
	"errors"
	"fmt"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/spill"
)

// Both HashSource adapters walk every hash once and stop at the callback's
// first error: the fence pin and the replicate adoption walk rely on that.
func TestHashSources_WalkAndStop(t *testing.T) {
	var hs []Hash
	set := spill.NewHashSet(spill.Options{MemEntries: 3, Dir: t.TempDir()})
	defer set.Close()
	for i := 0; i < 10; i++ {
		h := HashOf([]byte(fmt.Sprintf("s%d", i)))
		hs = append(hs, h)
		set.Add(h)
	}
	sp, _ := fenceWorld(t)
	cas := NewCAS(sp, WithSpilledAdoption(spill.Options{MemEntries: 3, Dir: t.TempDir()}))
	defer cas.CloseAdopted()
	for _, h := range hs {
		if err := cas.markAdopted(h); err != nil {
			t.Fatal(err)
		}
	}
	for name, src := range map[string]HashSource{"slice": sliceSource(hs), "spill": spillSource{set}, "adopted": cas.AdoptedSource()} {
		if src.Len() != len(hs) {
			t.Fatalf("%s: Len = %d, want %d", name, src.Len(), len(hs))
		}
		seen := map[Hash]bool{}
		if err := src.ForEachHash(func(h Hash) error { seen[h] = true; return nil }); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(seen) != len(hs) {
			t.Fatalf("%s: walked %d distinct hashes, want %d", name, len(seen), len(hs))
		}
		boom := errors.New("boom")
		calls := 0
		if err := src.ForEachHash(func(Hash) error { calls++; return boom }); !errors.Is(err, boom) || calls != 1 {
			t.Fatalf("%s: err=%v after %d calls, want boom after 1", name, err, calls)
		}
	}
}
