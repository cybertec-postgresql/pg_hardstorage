package cli

// scrub_window_test.go — a sampling scrub that never moves is not
// sampling.
//
// Both scrub commands describe themselves as sampling:
//
//	repo scrub    "samples N% of the repo's referenced chunks";
//	              default --sample-percent 1, "for hourly cron"
//	repair scrub  "samples N chunks (default 1000)"
//
// Neither did. The walk is fully deterministic — deployments sorted,
// manifests in key order (which is chronological), chunks in file order
// — and it simply stopped once `limit` chunks had been sampled. So
// every run examined the identical prefix: at the documented default,
// the oldest backups of the alphabetically-first deployment, an hour
// after hour, forever.
//
// What that means in practice is worse than "1% coverage". The newest
// backups — the ones an operator would actually restore from — were
// never re-hashed at all, and neither was any deployment after the
// first. Bit rot outside that fixed prefix was invisible to the only
// tool whose job is to find it, while the output reported a sample
// percent that reads as rotating coverage.
//
// scrubWindowStart makes successive runs advance through the repo, so
// ceil(total/limit) runs cover every referenced chunk exactly once.

import (
	"testing"
	"time"
)

func TestScrubWindowStart_ConsecutiveRunsCoverEverything(t *testing.T) {
	const total, limit = 1000, 10 // 1% per run → 100 windows

	seen := make(map[int]bool, total)
	var windows int
	for w := 0; w < 100; w++ {
		start, n := scrubWindowStart(total, limit, w)
		windows = n
		for i := start; i < start+limit && i < total; i++ {
			seen[i] = true
		}
	}
	if windows != 100 {
		t.Fatalf("windows = %d, want 100", windows)
	}
	if len(seen) != total {
		t.Fatalf("a full cycle of %d runs covered %d of %d chunks — the scrub still has a "+
			"permanent blind spot", windows, len(seen), total)
	}
}

// The bug in one assertion: two different runs must not examine the
// same slice.
func TestScrubWindowStart_SuccessiveRunsDoNotRepeat(t *testing.T) {
	const total, limit = 1000, 10
	a, _ := scrubWindowStart(total, limit, 7)
	b, _ := scrubWindowStart(total, limit, 8)
	if a == b {
		t.Fatalf("runs 7 and 8 both start at chunk %d — the scrub re-examines the same "+
			"chunks every run and never reaches the rest of the repo", a)
	}
}

// A cycle must wrap rather than run off the end or stall.
func TestScrubWindowStart_WrapsAfterAFullCycle(t *testing.T) {
	const total, limit = 1000, 10
	first, _ := scrubWindowStart(total, limit, 0)
	wrapped, _ := scrubWindowStart(total, limit, 100)
	if first != wrapped {
		t.Errorf("window 100 = %d, want the same as window 0 (%d)", wrapped, first)
	}
	last, n := scrubWindowStart(total, limit, 99)
	if last+limit != total || n != 100 {
		t.Errorf("final window starts at %d of %d (windows=%d); a cycle must end exactly at the end",
			last, total, n)
	}
}

// A run that covers everything anyway must not be perturbed: --full,
// limit=0, and repos smaller than the limit all start at 0.
func TestScrubWindowStart_FullScansAreUnaffected(t *testing.T) {
	cases := []struct{ total, limit, windowIndex int }{
		{1000, 0, 42},    // limit 0 = every chunk
		{1000, 1000, 42}, // limit == total
		{1000, 5000, 42}, // limit > total (tiny repo)
		{0, 10, 42},      // empty repo
	}
	for _, c := range cases {
		start, windows := scrubWindowStart(c.total, c.limit, c.windowIndex)
		if start != 0 || windows != 1 {
			t.Errorf("total=%d limit=%d: got (start=%d, windows=%d), want (0, 1)",
				c.total, c.limit, start, windows)
		}
	}
}

// A negative or wrapped clock must not produce a negative offset.
func TestScrubWindowStart_NegativeIndexStaysInRange(t *testing.T) {
	const total, limit = 1000, 10
	for _, w := range []int{-1, -99, -100, -101} {
		start, windows := scrubWindowStart(total, limit, w)
		if start < 0 || start >= total {
			t.Errorf("windowIndex=%d → start=%d, out of [0,%d)", w, start, total)
		}
		if windows != 100 {
			t.Errorf("windowIndex=%d → windows=%d, want 100", w, windows)
		}
	}
}

// The rotation is keyed to the hourly cron the help text recommends.
func TestScrubWindowIndex_AdvancesHourly(t *testing.T) {
	base := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	if a, b := scrubWindowIndex(base), scrubWindowIndex(base.Add(30*time.Minute)); a != b {
		t.Errorf("index moved within the same hour: %d vs %d", a, b)
	}
	if a, b := scrubWindowIndex(base), scrubWindowIndex(base.Add(time.Hour)); b != a+1 {
		t.Errorf("index %d → %d across an hour boundary, want %d", a, b, a+1)
	}
}
