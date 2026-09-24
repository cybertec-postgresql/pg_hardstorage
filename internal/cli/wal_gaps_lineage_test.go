package cli

import "testing"

// forkAt is a fixed fork lookup: newTLI's lineage leaves prevTLI inside
// segment seg.
func forkAt(newTLI, prevTLI uint32, seg uint64) timelineForkLookup {
	return func(n, p uint32) (uint64, bool) {
		if n == newTLI && p == prevTLI {
			return seg, true
		}
		return 0, false
	}
}

// TestFindLineageGaps_NewTimelineMustCoverFromItsFork is the detection
// half of the cross-timeline resume bug. TLI 1 is archived through
// segment 10 but TLI 2 forked inside segment 8; TLI 2 is archived only
// from 11. Segment numbers alone read 10 -> 11 as contiguous, yet TLI 2's
// segments 8..10 exist nowhere (TLI 1's 8..10 past the fork are a
// different, diverged history). A PITR along TLI 2 through that range
// reads a hole.
func TestFindLineageGaps_NewTimelineMustCoverFromItsFork(t *testing.T) {
	var segs []walSegment
	for n := uint64(0); n <= 10; n++ {
		segs = append(segs, walSegment{Timeline: 1, SegmentNumber: n})
	}
	segs = append(segs, walSegment{Timeline: 2, SegmentNumber: 11}, walSegment{Timeline: 2, SegmentNumber: 12})

	got := findLineageGaps(segs, forkAt(2, 1, 8))
	if len(got) != 1 {
		t.Fatalf("gaps = %+v, want exactly one: TLI 2 segments 8..10", got)
	}
	g := got[0]
	if g.Timeline != 2 || g.StartSegment != 8 || g.EndSegment != 10 || g.MissingCount != 3 {
		t.Errorf("gap = %+v, want TLI 2 #8..#10 (3 missing)", g)
	}

	// Covered from the fork segment: no gap.
	ok := append(segs[:11:11], walSegment{Timeline: 2, SegmentNumber: 8},
		walSegment{Timeline: 2, SegmentNumber: 9}, walSegment{Timeline: 2, SegmentNumber: 10},
		walSegment{Timeline: 2, SegmentNumber: 11})
	if got := findLineageGaps(ok, forkAt(2, 1, 8)); len(got) != 0 {
		t.Errorf("a new timeline covered from its fork segment reported gaps %+v", got)
	}
	// Unknown fork (no history): segment arithmetic only, as before.
	if got := findLineageGaps(segs, nil); len(got) != 0 {
		t.Errorf("no fork information must not invent gaps: %+v", got)
	}
}
