//go:build mutation_gap_timeline_skip

package cli

// findGaps — MUTATED variant: restores the pre-59816d8 timeline skip.
//
// This is not a synthetic fault; it is the exact code that shipped
// until 2026-08-07. Every timeline transition was skipped outright, so
// `wal audit` — and the chaos soak's "WAL lineage must be gap-free"
// gate, which runs it — was structurally blind to a hole straddling a
// failover, the one place an HA deployment is most likely to lose WAL.
//
// Caught by TestFindGaps_AcrossATimelineChange (the straddle cases) and
// TestWalAudit_HoleStraddlingAPromotionIsDetected.
func findGaps(segs []walSegment) []walGap {
	if len(segs) < 2 {
		return nil
	}
	var gaps []walGap
	for i := 1; i < len(segs); i++ {
		prev, curr := segs[i-1], segs[i]
		if prev.Timeline != curr.Timeline {
			continue
		}
		if curr.SegmentNumber > prev.SegmentNumber+1 {
			gaps = append(gaps, walGap{
				Timeline:     prev.Timeline,
				StartSegment: prev.SegmentNumber + 1,
				EndSegment:   curr.SegmentNumber - 1,
				MissingCount: curr.SegmentNumber - prev.SegmentNumber - 1,
			})
		}
	}
	return gaps
}
