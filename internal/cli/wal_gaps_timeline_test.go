package cli

// wal_gaps_timeline_test.go — `wal audit` must be able to see the hole
// a failover leaves.
//
// findGaps skipped every timeline transition:
//
//	if prev.Timeline != curr.Timeline {
//	    continue
//	}
//
// so it could only find holes INSIDE one timeline. A failover is a
// timeline transition, which put the blind spot exactly where an HA
// deployment is most likely to lose WAL — and the chaos soak's
// "the WAL lineage must be gap-free" gate runs `wal audit`, so the
// strongest automated check in the repository inherited it. It would
// have passed with an arbitrarily large hole straddling the bump.
//
// The skip was never necessary. Segment numbering is continuous across
// a promotion, so the ordinary arithmetic is already correct at the
// boundary — see the cases below, which pin both directions: a real
// hole is reported, and the two normal shapes (contiguous handover,
// and an old timeline holding diverged WAL past the branch point) are
// not.

import "testing"

// tseg is a segment on a timeline, for readability in the tables below.
func tseg(tli uint32, n uint64) walSegment {
	return walSegment{Timeline: tli, SegmentNumber: n}
}

func TestFindGaps_AcrossATimelineChange(t *testing.T) {
	cases := []struct {
		name  string
		segs  []walSegment
		want  []walGap
		notes string
	}{
		{
			name: "hole straddling a promotion is reported",
			// TLI 1 archived through #10; TLI 2's first segment is #50.
			// Segments 11..49 are held nowhere. This is the shape a
			// stream resume that anchored at the new leader's position
			// leaves behind.
			segs: []walSegment{tseg(1, 9), tseg(1, 10), tseg(2, 50), tseg(2, 51)},
			want: []walGap{{Timeline: 1, EndTimeline: 2, StartSegment: 11, EndSegment: 49, MissingCount: 39}},
		},
		{
			name: "contiguous handover is not a gap",
			// The new timeline's first archived segment is the one
			// containing the branch LSN, so it lands at or just after
			// the old timeline's last. This is the normal case and it
			// must stay silent.
			segs: []walSegment{tseg(1, 9), tseg(1, 10), tseg(2, 11), tseg(2, 12)},
			want: nil,
		},
		{
			name: "same-segment handover is not a gap",
			// The branch segment is rewritten on the new timeline, so
			// the same number legitimately appears on both.
			segs: []walSegment{tseg(1, 10), tseg(2, 10), tseg(2, 11)},
			want: nil,
		},
		{
			name: "overlap is not a gap",
			// TLI 1 kept writing past the branch point before the old
			// primary was fenced. Those segments are diverged history,
			// not something missing — reporting them would send an
			// operator hunting for WAL that must never be replayed.
			segs: []walSegment{tseg(1, 40), tseg(2, 11), tseg(2, 12)},
			want: nil,
		},
		{
			name: "an ordinary same-timeline gap still reports without EndTimeline",
			segs: []walSegment{tseg(1, 3), tseg(1, 9)},
			want: []walGap{{Timeline: 1, StartSegment: 4, EndSegment: 8, MissingCount: 5}},
		},
		{
			name: "holes on both sides of a promotion",
			segs: []walSegment{tseg(1, 1), tseg(1, 5), tseg(2, 30), tseg(2, 40)},
			want: []walGap{
				{Timeline: 1, StartSegment: 2, EndSegment: 4, MissingCount: 3},
				{Timeline: 1, EndTimeline: 2, StartSegment: 6, EndSegment: 29, MissingCount: 24},
				{Timeline: 2, StartSegment: 31, EndSegment: 39, MissingCount: 9},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := findGaps(tc.segs)
			if len(got) != len(tc.want) {
				t.Fatalf("found %d gap(s), want %d\ngot:  %+v\nwant: %+v", len(got), len(tc.want), got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("gap[%d] = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestWalGap_DescribeNamesTheTimelineChange: the straddle has to be
// visible in text mode, not only in the JSON. It points at a different
// cause and a different remedy than a hole inside one timeline, and an
// operator reading `wal audit` output is the person who needs to know
// which one they have.
func TestWalGap_DescribeNamesTheTimelineChange(t *testing.T) {
	straddle := walGap{Timeline: 1, EndTimeline: 2, StartSegment: 11, EndSegment: 49, MissingCount: 39}
	got := straddle.describe()
	for _, want := range []string{"1->2", "timeline change", "39 missing"} {
		if !contains(got, want) {
			t.Errorf("describe() = %q, missing %q", got, want)
		}
	}

	plain := walGap{Timeline: 1, StartSegment: 4, EndSegment: 8, MissingCount: 5}
	if p := plain.describe(); contains(p, "timeline change") || contains(p, "->") {
		t.Errorf("a same-timeline gap describes itself as a timeline change: %q", p)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestFindGaps_CrossTimelineResumeHandoffIsNotAGap pins the archive
// shape the cross-timeline resume produces against the gap detector.
//
// When a streamer falls behind and walks the timeline chain back up,
// each timeline contributes only the WHOLE segments it had left
// before its branch point, and the next timeline continues from the
// segment containing that branch — because PostgreSQL copies the old
// timeline's last partial segment under the new timeline's name when
// it promotes. findGaps documents exactly this as the no-gap case
// ("the new timeline's first archived segment is the one containing
// the branch LSN, so it lands at or just after the old timeline's
// last"), and the resume walk is the code that has to keep it true.
//
// Getting this wrong is silent in the worst way: an archive that
// looks complete to `wal audit` but is not, or a correct archive that
// trips the gap gate on every failover.
func TestFindGaps_CrossTimelineResumeHandoffIsNotAGap(t *testing.T) {
	// The chaos gate's geometry: timeline 27 ended inside segment A3,
	// timeline 28 inside A6, live on 29.
	segs := []walSegment{
		{Timeline: 27, SegmentNumber: 0xA1},
		{Timeline: 27, SegmentNumber: 0xA2},
		{Timeline: 28, SegmentNumber: 0xA3},
		{Timeline: 28, SegmentNumber: 0xA4},
		{Timeline: 28, SegmentNumber: 0xA5},
		{Timeline: 29, SegmentNumber: 0xA6},
		{Timeline: 29, SegmentNumber: 0xA7},
	}
	if got := findGaps(segs); len(got) != 0 {
		t.Fatalf("the cross-timeline resume hand-off was reported as %d gap(s): %+v\n"+
			"This shape is what a streamer walking the timeline chain produces, and "+
			"findGaps documents it as the contiguous case.", len(got), got)
	}
}

// The counterpart: a promotion that genuinely lost WAL must still be
// caught. If the hand-off case above were made gap-free by loosening
// the check rather than by the archive being correct, this would go
// quiet too — and a hole straddling a failover is the one an HA
// deployment is most likely to have.
func TestFindGaps_LostWALAcrossAPromotionIsStillAGap(t *testing.T) {
	segs := []walSegment{
		{Timeline: 27, SegmentNumber: 0xA1},
		{Timeline: 27, SegmentNumber: 0xA2},
		// A3, A4 never archived — the streamer was dead across the
		// promotion and resumed too late.
		{Timeline: 28, SegmentNumber: 0xA5},
	}
	got := findGaps(segs)
	if len(got) != 1 {
		t.Fatalf("expected exactly one gap, got %d: %+v", len(got), got)
	}
	if got[0].StartSegment != 0xA3 || got[0].EndSegment != 0xA4 {
		t.Errorf("gap = segments %X..%X, want A3..A4", got[0].StartSegment, got[0].EndSegment)
	}
}
