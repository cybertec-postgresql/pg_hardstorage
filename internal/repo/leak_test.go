package repo

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/spill"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/leakcheck"
)

// The GC / reference-collection / scrub walkers are run by agents and
// cron jobs; run each many times in one process (dry-run, apply, failing,
// cancelled) with a spilled reference set and check nothing accumulates:
// spill dirs, fds, goroutines, heap, and the repo's own gc-run records.
func TestLeak_GCSweepAndCollectRepeated(t *testing.T) {
	sp, root := fenceWorld(t)
	var live []Hash
	for i := 0; i < 60; i++ {
		live = append(live, oldOrphan(t, sp, root, fmt.Sprintf("live-%d", i)))
	}
	commitRefManifest(t, sp, "b1", live[:40]...)
	commitRefManifest(t, sp, "b2", live[20:]...)
	spillDir := t.TempDir()
	n := 60
	if os.Getenv("PGHS_SCALE") == "1" {
		n = 600
	}
	step := func(i int) {
		ctx := context.Background()
		switch i % 4 {
		case 0, 1:
			res, err := Sweep(ctx, sp, SweepOptions{Apply: i%4 == 1, RefSpill: spill.Options{MemEntries: 5, Dir: spillDir}})
			if err != nil {
				t.Fatal(err)
			}
			if res.RefCount != 60 {
				t.Fatalf("refcount %d", res.RefCount)
			}
		case 2:
			c, cancel := context.WithCancel(ctx)
			cancel()
			if _, err := Sweep(c, sp, SweepOptions{RefSpill: spill.Options{MemEntries: 5, Dir: spillDir}}); err == nil {
				t.Fatal("cancelled sweep succeeded")
			}
		case 3:
			refs, err := CollectReferencesWithOptions(ctx, sp, CollectReferencesOptions{RefSpill: spill.Options{MemEntries: 5, Dir: spillDir}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Scrub(ctx, NewCAS(sp), refs, 0); err != nil {
				t.Fatal(err)
			}
			if _, err := FindMissing(ctx, sp, refs); err != nil {
				t.Fatal(err)
			}
			_ = refs.Close()
		}
	}
	for i := 0; i < 4; i++ {
		step(i)
	}
	base := leakcheck.Take()
	gcRecords := len(leakcheck.DirEntries(root + "/gc"))
	leakcheck.Plateau(t, n, n/16, 8<<20, step)
	if left := leakcheck.DirEntries(spillDir); len(left) != 0 {
		t.Fatalf("spill dirs left behind: %v", left)
	}
	if got := len(leakcheck.DirEntries(root + "/gc")); got > gcRecords+2 {
		t.Errorf("gc run records accumulate in the repo: %d -> %d", gcRecords, got)
	}
	leakcheck.AssertNoLeak(t, base, leakcheck.Tolerance{})
}
