package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
)

// The scan keeps 16 bytes per archived segment (timeline + number); the
// name/LSN strings are only built for the rows that are printed.
func TestScanWALSegments_CompactAndRowsMatchNames(t *testing.T) {
	const size = 64 << 20
	sp := segSizeWorld(t, 25, size)

	segs, gotSize, err := scanWALSegments(context.Background(), sp, "db1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if gotSize != size {
		t.Fatalf("segment size = %d, want %d", gotSize, int64(size))
	}
	if len(segs) != 25 {
		t.Fatalf("got %d segments, want 25", len(segs))
	}
	for i, sg := range segs {
		if sg.Timeline != 1 || sg.SegmentNumber != uint64(i) {
			t.Fatalf("segs[%d] = %+v", i, sg)
		}
	}
	rows := walSegmentRows(segs[:3], gotSize)
	for i, r := range rows {
		want := walsink.SegmentFileName(1, uint64(i), size)
		if r.SegmentName != want {
			t.Fatalf("row %d name = %q, want %q", i, r.SegmentName, want)
		}
	}
	if rows[1].StartLSN != "0/4000000" || rows[1].EndLSN != "0/8000000" {
		t.Fatalf("row 1 LSNs = %s..%s", rows[1].StartLSN, rows[1].EndLSN)
	}
}

func TestWalListBody_TruncationNotice(t *testing.T) {
	b := walListBody{
		Deployment:        "db1",
		Timelines:         []walTimelineSummary{{Timeline: 1, SegmentCount: 500, LowestSegment: 0, HighestSegment: 499}},
		Segments:          walSegmentRows([]walSegment{{Timeline: 1, SegmentNumber: 0}}, 16<<20),
		SegmentCount:      500,
		SegmentsTruncated: true,
	}
	var sb strings.Builder
	if err := b.WriteText(&sb); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sb.String(), "1 of 500 segments shown") {
		t.Fatalf("missing truncation notice:\n%s", sb.String())
	}
}
