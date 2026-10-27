package repo

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"
)

func sortedOrphans(t *testing.T, n int, mk func(i int) Hash) []Hash {
	t.Helper()
	hs := make([]Hash, n)
	for i := range hs {
		hs[i] = mk(i)
	}
	sort.Slice(hs, func(i, j int) bool { return hs[i].Compare(hs[j]) < 0 })
	return hs
}

// One checkpoint covers many slices when BatchDuration allows: the rescan
// runs once, yet a manifest committed before that rescan still spares its
// chunk and everything else goes.
func TestSweep_OneCheckpointCoversManySlices(t *testing.T) {
	sp, root := fenceWorld(t)
	orphans := sortedOrphans(t, 14, func(i int) Hash { return oldOrphan(t, sp, root, fmt.Sprintf("o%d", i)) })
	protect := orphans[13] // lives in the LAST slice
	checkpoints := 0
	res, err := Sweep(context.Background(), sp, SweepOptions{
		Apply: true, BatchSize: 4, BatchDuration: time.Hour,
		beforeBatch: func(int) {
			checkpoints++
			commitRefManifest(t, sp, "late", protect)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if checkpoints != 1 {
		t.Errorf("checkpoints = %d, want 1 (one rescan for all slices)", checkpoints)
	}
	if res.Deleted != 13 || res.SkippedReferenced != 1 || !chunkExists(t, sp, protect) {
		t.Fatalf("Deleted=%d Skipped=%d protectedExists=%v", res.Deleted, res.SkippedReferenced, chunkExists(t, sp, protect))
	}
}

// With a checkpoint per slice, a manifest committed between checkpoints
// (before the one that precedes the chunk's own slice) still protects it.
func TestSweep_ManifestBetweenCheckpointsProtectsLaterSlice(t *testing.T) {
	sp, root := fenceWorld(t)
	orphans := sortedOrphans(t, 14, func(i int) Hash { return oldOrphan(t, sp, root, fmt.Sprintf("o%d", i)) })
	protect := orphans[13]
	checkpoints := 0
	res, err := Sweep(context.Background(), sp, SweepOptions{
		Apply: true, BatchSize: 4, BatchDuration: -1,
		beforeBatch: func(b int) {
			checkpoints++
			if b == 2 {
				commitRefManifest(t, sp, "mid", protect)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if checkpoints != 4 {
		t.Errorf("checkpoints = %d, want 4", checkpoints)
	}
	if !chunkExists(t, sp, protect) || res.SkippedReferenced != 1 || res.Deleted != 13 {
		t.Fatalf("exists=%v skipped=%d deleted=%d", chunkExists(t, sp, protect), res.SkippedReferenced, res.Deleted)
	}
}

// The checkpoint window is capped by the writer fence wait: with a tiny
// GCFenceMaxWait every slice gets its own checkpoint despite BatchDuration.
func TestSweep_CheckpointWindowCappedByFenceMaxWait(t *testing.T) {
	sp, root := fenceWorld(t)
	for i := 0; i < 9; i++ {
		oldOrphan(t, sp, root, fmt.Sprintf("o%d", i))
	}
	GCFenceMaxWait = time.Millisecond
	checkpoints := 0
	res, err := Sweep(context.Background(), sp, SweepOptions{
		Apply: true, BatchSize: 3, BatchDuration: time.Hour,
		beforeBatch: func(int) { checkpoints++ },
	})
	if err != nil {
		t.Fatal(err)
	}
	if checkpoints != 3 || res.Deleted != 9 {
		t.Errorf("checkpoints=%d deleted=%d, want 3/9", checkpoints, res.Deleted)
	}
}

// Defaults (no BatchSize) are adaptive: a small sweep is a single checkpoint.
func TestSweep_DefaultIsAdaptive(t *testing.T) {
	sp, root := fenceWorld(t)
	for i := 0; i < 10; i++ {
		oldOrphan(t, sp, root, fmt.Sprintf("o%d", i))
	}
	n := 0
	if _, err := Sweep(context.Background(), sp, SweepOptions{Apply: true, beforeBatch: func(int) { n++ }}); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("checkpoints = %d, want 1", n)
	}
}

func TestScrubResult_MismatchListCappedCountExact(t *testing.T) {
	var r ScrubResult
	for i := 0; i < ScrubMismatchListLimit+7; i++ {
		r.noteMismatch(HashOf([]byte(fmt.Sprintf("x%d", i))))
	}
	if r.MismatchCount != ScrubMismatchListLimit+7 || len(r.Mismatches) != ScrubMismatchListLimit {
		t.Fatalf("count=%d listed=%d", r.MismatchCount, len(r.Mismatches))
	}
}
