package backup

import (
	"fmt"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// With the per-side sorter forced to spill after a few KiB, Compare must give
// the same answer as the in-memory path: the merge-join over spilled runs is
// the whole file-level diff.
func TestCompare_FileIndexSpillsAndStillJoins(t *testing.T) {
	old := compareSortMemBytes
	compareSortMemBytes = 2 << 10
	defer func() { compareSortMemBytes = old }()

	mk := func(n int, mut func(i int) (changed bool, skip bool)) *Manifest {
		m := &Manifest{BackupID: "x"}
		for i := 0; i < n; i++ {
			ch, skip := false, false
			if mut != nil {
				ch, skip = mut(i)
			}
			if skip {
				continue
			}
			h := repo.HashOf([]byte(fmt.Sprintf("c%d", i)))
			if ch {
				h = repo.HashOf([]byte(fmt.Sprintf("changed%d", i)))
			}
			// Insert out of path order on purpose: the sorter orders them.
			m.Files = append([]FileEntry{{Path: fmt.Sprintf("base/%05d", i), Size: 10,
				Chunks: []ChunkRef{{Hash: h, Offset: 0, Len: 10}}}}, m.Files...)
		}
		return m
	}
	const n = 3000
	a := mk(n, nil)
	b := mk(n+100, func(i int) (bool, bool) { return i%10 == 0, i%25 == 1 && i < n })
	res, err := Compare(a, b, CompareOptions{})
	if err != nil {
		t.Fatal(err)
	}
	removed := 0
	for i := 0; i < n; i++ {
		if i%25 == 1 {
			removed++
		}
	}
	changed := 0
	for i := 0; i < n; i++ {
		if i%10 == 0 && !(i%25 == 1) {
			changed++
		}
	}
	if res.FileCounts.OnlyInA != removed || res.FileCounts.OnlyInB != 100 ||
		res.FileCounts.InBoth != n-removed || res.FileCounts.Changed != changed {
		t.Fatalf("file counts %+v; want onlyA=%d onlyB=100 both=%d changed=%d", res.FileCounts, removed, n-removed, changed)
	}
}
