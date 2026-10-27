package repo

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/spill"
)

// A reference set forced onto disk (MemEntries=3) must give the same GC
// verdicts as the heap one: referenced chunks survive, orphans go.
func TestSweep_SpilledRefSetKeepsReferencedDeletesOrphans(t *testing.T) {
	sp, root := fenceWorld(t)
	var live []Hash
	for i := 0; i < 40; i++ {
		live = append(live, oldOrphan(t, sp, root, fmt.Sprintf("live-%d", i)))
	}
	commitRefManifest(t, sp, "b1", live[:20]...)
	commitRefManifest(t, sp, "b2", live[10:]...)
	var orphans []Hash
	for i := 0; i < 15; i++ {
		orphans = append(orphans, oldOrphan(t, sp, root, fmt.Sprintf("orphan-%d", i)))
	}
	spillDir := t.TempDir()
	res, err := Sweep(context.Background(), sp, SweepOptions{
		Apply:    true,
		RefSpill: spill.Options{MemEntries: 3, Dir: spillDir},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.RefCount != 40 {
		t.Errorf("RefCount = %d, want 40", res.RefCount)
	}
	if res.Deleted != len(orphans) {
		t.Errorf("Deleted = %d, want %d", res.Deleted, len(orphans))
	}
	for _, h := range live {
		if !chunkExists(t, sp, h) {
			t.Fatalf("referenced chunk %s was deleted", h)
		}
	}
	for _, h := range orphans {
		if chunkExists(t, sp, h) {
			t.Fatalf("orphan %s survived", h)
		}
	}
}

func TestCollectReferences_SpilledSetIteratesSorted(t *testing.T) {
	sp, root := fenceWorld(t)
	var hs []Hash
	for i := 0; i < 30; i++ {
		hs = append(hs, oldOrphan(t, sp, root, fmt.Sprintf("c-%d", i)))
	}
	commitRefManifest(t, sp, "b1", hs...)
	refs, err := CollectReferencesWithOptions(context.Background(), sp, CollectReferencesOptions{
		RefSpill: spill.Options{MemEntries: 4, Dir: t.TempDir()},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer refs.Close()
	var prev Hash
	n := 0
	if err := refs.ForEach(func(h Hash) error {
		if n > 0 && h.Compare(prev) <= 0 {
			t.Fatalf("not strictly ascending at %d", n)
		}
		prev = h
		n++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if n != 30 {
		t.Fatalf("visited %d, want 30", n)
	}
}

func TestStreamBackupChunkRefs(t *testing.T) {
	h := strings.Repeat("ab", 32)
	cases := []struct {
		name, doc string
		want      int
		wantErr   bool
	}{
		{"plain", `{"schema":"x","files":[{"path":"a","chunks":[{"hash":"` + h + `","offset":0,"len":1},{"hash":"` + h + `"}]}]}`, 2, false},
		{"files after other fields", `{"dirs":[{"path":"d"}],"files":[{"chunks":[{"hash":"` + h + `"}]}],"wal_required":["x"]}`, 1, false},
		{"case-folded keys still bind", `{"Files":[{"Chunks":[{"hash":"` + h + `"}]}]}`, 1, false},
		{"repeated files key counts both", `{"files":[{"chunks":[{"hash":"` + h + `"}]}],"files":[{"chunks":[{"hash":"` + h + `"}]}]}`, 2, false},
		{"null files", `{"files":null}`, 0, false},
		{"null chunks", `{"files":[{"chunks":null}]}`, 0, false},
		{"empty", `{}`, 0, false},
		{"truncated", `{"files":[{"chunks":[{"hash":"` + h + `"},`, 1, true},
		{"files not array", `{"files":3}`, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := 0
			err := streamBackupChunkRefs(strings.NewReader(c.doc), func(string) error { n++; return nil })
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if n != c.want {
				t.Fatalf("visited %d refs, want %d", n, c.want)
			}
		})
	}
}
