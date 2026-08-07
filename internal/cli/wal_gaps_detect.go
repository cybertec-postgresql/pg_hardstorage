//go:build !mutation_gap_timeline_skip

package cli

// findGaps reports contiguous missing-segment ranges.
// The list is sorted by (timeline, start_segment).
//
// Timeline changes are INCLUDED. They used to be skipped outright — a
// `continue` on prev.Timeline != curr.Timeline — which made the check
// structurally unable to see the one hole an HA deployment is most
// likely to have. A failover is a timeline change, so a promotion that
// loses WAL puts the hole exactly where nothing was looking. The chaos
// soak's "the WAL lineage must be gap-free" gate runs `wal audit`, so
// it inherited the blind spot and would have passed with an
// arbitrarily large hole straddling the bump.
//
// The same arithmetic is correct across the boundary, which is why the
// skip was never needed. Segment numbering is continuous across a
// promotion: the new timeline's first archived segment is the one
// containing the branch LSN, so it lands at or just after the old
// timeline's last — `curr <= prev+1`, no gap reported. A new timeline
// that starts well past prev+1 is a genuine hole.
//
// Overlap is not a gap either. The old timeline can hold segments PAST
// the branch point, written by a primary that kept going before it was
// fenced; then curr < prev and the condition is simply false. That WAL
// is diverged history rather than something missing, and reporting it
// as a gap would send an operator hunting for segments that should not
// be replayed.
func findGaps(segs []walSegment) []walGap {
	if len(segs) < 2 {
		return nil
	}
	var gaps []walGap
	for i := 1; i < len(segs); i++ {
		prev, curr := segs[i-1], segs[i]
		if curr.SegmentNumber > prev.SegmentNumber+1 {
			g := walGap{
				Timeline:     prev.Timeline,
				StartSegment: prev.SegmentNumber + 1,
				EndSegment:   curr.SegmentNumber - 1,
				MissingCount: curr.SegmentNumber - prev.SegmentNumber - 1,
			}
			if curr.Timeline != prev.Timeline {
				g.EndTimeline = curr.Timeline
			}
			gaps = append(gaps, g)
		}
	}
	return gaps
}
