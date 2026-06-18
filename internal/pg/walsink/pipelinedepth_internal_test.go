package walsink

import "testing"

// TestPipelineDepthFor bounds the upfront segment-buffer allocation:
// depth*segSize must stay near the 256 MiB budget (never the 16*seg a
// fixed depth would give — which is 16 GiB for 1 GiB segments), while
// keeping at least 2 buffers for pipelining.
func TestPipelineDepthFor(t *testing.T) {
	cases := []struct {
		seg       int64
		wantDepth int
	}{
		{16 << 20, 16}, // default → full depth (256 MiB pool)
		{32 << 20, 8},
		{64 << 20, 4},
		{128 << 20, 2},
		{256 << 20, 2}, // budget/seg = 1 → floored to 2
		{1 << 30, 2},   // 1 GiB → floored to 2 (2 GiB pool, the overlap minimum)
		{1 << 20, 16},  // 1 MiB → budget/seg = 256, capped to 16
		{0, 16},        // 0 → default 16 MiB → 16
	}
	for _, c := range cases {
		got := pipelineDepthFor(c.seg)
		if got != c.wantDepth {
			t.Errorf("pipelineDepthFor(%d) = %d, want %d", c.seg, got, c.wantDepth)
		}
		pool := int64(got) * NormSegmentSize(c.seg)
		// When two buffers fit the budget (seg ≤ budget/2) the pool must
		// stay within budget. Above that the 2-buffer floor dominates and
		// the pool is exactly 2*seg (≤ 2 GiB at the 1 GiB max segment).
		if NormSegmentSize(c.seg) <= pipelineBufferBudget/2 {
			if pool > pipelineBufferBudget {
				t.Errorf("seg=%d: pool=%d exceeds budget %d", c.seg, pool, pipelineBufferBudget)
			}
		} else if pool > 2*MaxSegmentSize {
			t.Errorf("seg=%d: pool=%d exceeds 2*MaxSegmentSize %d", c.seg, pool, 2*MaxSegmentSize)
		}
		if got < 2 {
			t.Errorf("seg=%d: depth=%d < 2 (no pipelining)", c.seg, got)
		}
	}
}
