package repo

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/spill"
)

// The orphan list must not grow the heap with the number of orphans: past
// sweepOrphanMemRecords it goes to a temp file, is read back in batches, and
// the file is gone afterwards. Every orphan is still deleted, every
// referenced chunk survives, and the report carries exact counts.
func TestSweep_OrphanLogSpillsAndIsCleanedUp(t *testing.T) {
	sp, root := fenceWorld(t)
	saved := sweepOrphanMemRecords
	sweepOrphanMemRecords = 3
	t.Cleanup(func() { sweepOrphanMemRecords = saved })

	var live []Hash
	for i := 0; i < 5; i++ {
		live = append(live, oldOrphan(t, sp, root, fmt.Sprintf("live-%d", i)))
	}
	commitRefManifest(t, sp, "b1", live...)
	var orphans []Hash
	for i := 0; i < 14; i++ {
		orphans = append(orphans, oldOrphan(t, sp, root, fmt.Sprintf("orphan-%d", i)))
	}
	dir := t.TempDir()
	batches := 0
	res, err := Sweep(context.Background(), sp, SweepOptions{
		Apply:       true,
		BatchSize:   4,
		RefSpill:    spill.Options{Dir: dir},
		beforeBatch: func(int) { batches++ },
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.OrphanCount != len(orphans) || res.Deleted != len(orphans) {
		t.Fatalf("OrphanCount=%d Deleted=%d, want %d", res.OrphanCount, res.Deleted, len(orphans))
	}
	if batches != 4 { // ceil(14/4)
		t.Errorf("ran %d batches, want 4", batches)
	}
	for _, h := range orphans {
		if chunkExists(t, sp, h) {
			t.Fatalf("orphan %s survived", h)
		}
	}
	for _, h := range live {
		if !chunkExists(t, sp, h) {
			t.Fatalf("referenced chunk %s was deleted", h)
		}
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Errorf("spill dir not cleaned up: %d entries left", len(left))
	}
	if !sort.SliceIsSorted(res.Orphans, func(i, j int) bool { return res.Orphans[i].Compare(res.Orphans[j]) < 0 }) {
		t.Error("Orphans sample is not sorted")
	}
}

func TestOrphanLog_BatchesCoverEverythingOnce(t *testing.T) {
	saved := sweepOrphanMemRecords
	sweepOrphanMemRecords = 5
	t.Cleanup(func() { sweepOrphanMemRecords = saved })
	for _, n := range []int{0, 1, 4, 5, 6, 11, 23} {
		l := &orphanLog{dir: t.TempDir()}
		want := map[Hash]int64{}
		for i := 0; i < n; i++ {
			h := HashOf([]byte(fmt.Sprintf("o%d", i)))
			want[h] = int64(i + 100)
			if err := l.add(orphanInfo{hash: h, size: int64(i + 100)}); err != nil {
				t.Fatal(err)
			}
		}
		got := map[Hash]int64{}
		if err := l.batches(4, func(b []orphanInfo) error {
			if len(b) == 0 || len(b) > 4 {
				t.Fatalf("n=%d: batch of %d", n, len(b))
			}
			for _, o := range b {
				if _, dup := got[o.hash]; dup {
					t.Fatalf("n=%d: %s delivered twice", n, o.hash)
				}
				got[o.hash] = o.size
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if len(got) != len(want) {
			t.Fatalf("n=%d: delivered %d, want %d", n, len(got), len(want))
		}
		for h, sz := range want {
			if got[h] != sz {
				t.Fatalf("n=%d: %s size %d, want %d", n, h, got[h], sz)
			}
		}
		l.close()
	}
}

// A dry run reports exact counts but keeps no orphan list at all.
func TestSweep_DryRunCountsWithoutLogging(t *testing.T) {
	sp, root := fenceWorld(t)
	saved := sweepOrphanMemRecords
	sweepOrphanMemRecords = 1
	t.Cleanup(func() { sweepOrphanMemRecords = saved })
	for i := 0; i < 6; i++ {
		oldOrphan(t, sp, root, fmt.Sprintf("o-%d", i))
	}
	dir := t.TempDir()
	res, err := Sweep(context.Background(), sp, SweepOptions{RefSpill: spill.Options{Dir: dir}})
	if err != nil {
		t.Fatal(err)
	}
	if res.OrphanCount != 6 || len(res.Orphans) != 6 || res.Deleted != 0 {
		t.Fatalf("OrphanCount=%d len(Orphans)=%d Deleted=%d", res.OrphanCount, len(res.Orphans), res.Deleted)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Errorf("dry run left %d spill entries", len(left))
	}
}

func TestHashSample_KeepsSmallest(t *testing.T) {
	s := hashSample{limit: 5}
	var all []Hash
	for i := 0; i < 200; i++ {
		h := HashOf([]byte(fmt.Sprintf("h%d", i)))
		all = append(all, h)
		s.offer(h)
		if len(s.buf) >= 2*s.limit+1 {
			t.Fatalf("buffer grew to %d", len(s.buf))
		}
	}
	sortHashes(all)
	got := s.sorted()
	if len(got) != 5 {
		t.Fatalf("len = %d", len(got))
	}
	for i := range got {
		if got[i] != all[i] {
			t.Fatalf("sample[%d] = %s, want %s", i, got[i], all[i])
		}
	}
}

func TestSweepResult_FailureListIsCapped(t *testing.T) {
	var r SweepResult
	for i := 0; i < maxSweepFailures+50; i++ {
		r.noteFailure("x")
	}
	if r.FailureCount != maxSweepFailures+50 || len(r.Failures) != maxSweepFailures {
		t.Fatalf("count=%d kept=%d", r.FailureCount, len(r.Failures))
	}
}

func TestFindOrphansLimit_AndFindMissingLimit(t *testing.T) {
	sp, root := fenceWorld(t)
	var orphans []Hash
	for i := 0; i < 9; i++ {
		orphans = append(orphans, oldOrphan(t, sp, root, fmt.Sprintf("orph-%d", i)))
	}
	sortHashes(orphans)
	refs := NewRefSet()
	defer refs.Close()
	got, total, err := FindOrphansLimit(context.Background(), sp, refs, FindOrphansOptions{MinAge: -1}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if total != 9 || len(got) != 4 {
		t.Fatalf("total=%d len=%d", total, len(got))
	}
	for i := range got {
		if got[i] != orphans[i] {
			t.Fatalf("got[%d]=%s want %s", i, got[i], orphans[i])
		}
	}
	all, _, _ := FindOrphansLimit(context.Background(), sp, refs, FindOrphansOptions{MinAge: -1}, -1)
	if len(all) != 9 {
		t.Fatalf("unlimited returned %d", len(all))
	}
	none, n, _ := FindOrphansLimit(context.Background(), sp, refs, FindOrphansOptions{MinAge: -1}, 0)
	if len(none) != 0 || n != 9 {
		t.Fatalf("limit 0: len=%d total=%d", len(none), n)
	}

	// Missing: reference chunks that are not stored.
	mrefs := NewRefSet()
	defer mrefs.Close()
	for i := 0; i < 7; i++ {
		mrefs.Add(HashOf([]byte(fmt.Sprintf("absent-%d", i))))
	}
	miss, mtotal, err := FindMissingLimit(context.Background(), sp, mrefs, 3)
	if err != nil {
		t.Fatal(err)
	}
	if mtotal != 7 || len(miss) != 3 {
		t.Fatalf("missing total=%d len=%d", mtotal, len(miss))
	}
}

// Harvest memo: a manifest is read once per (key,size,mtime) however many
// scans run, and a rewritten manifest is read again.
func TestManifestSnapshot_HarvestMemoIsIncremental(t *testing.T) {
	sp, root := fenceWorld(t)
	a := oldOrphan(t, sp, root, "a")
	b := oldOrphan(t, sp, root, "b")
	commitRefManifest(t, sp, "b1", a)
	counting := &countingGets{StoragePlugin: sp}
	snap := newManifestSnapshot(spill.Options{Dir: t.TempDir()}, CollectReferencesOptions{})
	defer snap.close()
	ctx := context.Background()
	if err := snap.scan(ctx, counting, false); err != nil {
		t.Fatal(err)
	}
	if err := snap.scan(ctx, counting, false); err != nil {
		t.Fatal(err)
	}
	if counting.gets != 1 {
		t.Fatalf("manifest read %d times over two scans, want 1", counting.gets)
	}
	commitRefManifest(t, sp, "b2", b)
	if err := snap.scan(ctx, counting, false); err != nil {
		t.Fatal(err)
	}
	if counting.gets != 2 {
		t.Fatalf("after one new manifest: %d reads, want 2", counting.gets)
	}
	if !snap.refs.Has(a) || !snap.refs.Has(b) {
		t.Fatal("reference set missing a harvested chunk")
	}
}

type countingGets struct {
	storage.StoragePlugin
	gets int
}

func (c *countingGets) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if strings.HasPrefix(key, "manifests/") {
		c.gets++
	}
	return c.StoragePlugin.Get(ctx, key)
}

func TestStreamSegmentChunkRefs(t *testing.T) {
	h := strings.Repeat("cd", 32)
	cases := []struct {
		name, doc string
		want      int
		wantErr   bool
	}{
		{"plain", `{"end_lsn":"0/1","chunks":[{"hash":"` + h + `","len":3},{"hash":"` + h + `"}]}`, 2, false},
		{"chunks first", `{"chunks":[{"hash":"` + h + `"}],"created_at":"x"}`, 1, false},
		{"case folded", `{"Chunks":[{"hash":"` + h + `"}]}`, 1, false},
		{"null", `{"chunks":null}`, 0, false},
		{"none", `{"end_lsn":"0/1"}`, 0, false},
		{"truncated", `{"chunks":[{"hash":"` + h + `"},`, 1, true},
		{"not array", `{"chunks":7}`, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := 0
			err := streamSegmentChunkRefs(strings.NewReader(c.doc), func(string) error { n++; return nil })
			if (err != nil) != c.wantErr || n != c.want {
				t.Fatalf("err=%v n=%d, want err=%v n=%d", err, n, c.wantErr, c.want)
			}
		})
	}
}

// WAL and logical-stream manifests are harvested as streams too: their
// chunks still land in the reference set.
func TestHarvestManifest_WALSegmentIsStreamed(t *testing.T) {
	sp, root := fenceWorld(t)
	h := oldOrphan(t, sp, root, "walchunk")
	body := `{"end_lsn":"0/1","chunks":[{"hash":"` + h.String() + `","len":1}]}`
	if err := putJSONRaw(sp, "wal/db1/00000001/000000010000000000000001.json", body); err != nil {
		t.Fatal(err)
	}
	refs, err := CollectReferences(context.Background(), sp)
	if err != nil {
		t.Fatal(err)
	}
	defer refs.Close()
	if !refs.Has(h) || refs.Len() != 1 {
		t.Fatalf("WAL chunk not referenced (len %d)", refs.Len())
	}
}
