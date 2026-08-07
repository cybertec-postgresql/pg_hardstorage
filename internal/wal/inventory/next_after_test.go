package inventory_test

import (
	"context"
	"testing"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/inventory"
)

// segStartLSN is the default-16MiB start LSN of segNum.
func segStartLSN(segNum uint64) pglogrepl.LSN {
	return pglogrepl.LSN(segNum * uint64(walsink.SegmentSize))
}

// TestNextArchivedLSNAtOrAfter_FindsResumePoint: with segments 8 and 9
// present, asking from inside the hole (segment 3) must answer where
// the archive resumes — segment 8's start — not segment 9's.
func TestNextArchivedLSNAtOrAfter_FindsResumePoint(t *testing.T) {
	sp := newSP(t)
	putSegmentManifest(t, sp, "db1", 1, walsink.SegmentFileName(1, 8, walsink.SegmentSize))
	putSegmentManifest(t, sp, "db1", 1, walsink.SegmentFileName(1, 9, walsink.SegmentSize))

	got, found, err := inventory.NextArchivedLSNAtOrAfter(context.Background(), sp, "db1", 1, segStartLSN(3))
	if err != nil || !found {
		t.Fatalf("found=%v err=%v, want a resume point", found, err)
	}
	if want := segStartLSN(8); got != want {
		t.Errorf("resume = %s, want %s", got, want)
	}
}

// TestNextArchivedLSNAtOrAfter_ExactBoundaryCounts: a segment starting
// exactly AT from is "at or after" — the window ends where it begins.
func TestNextArchivedLSNAtOrAfter_ExactBoundaryCounts(t *testing.T) {
	sp := newSP(t)
	putSegmentManifest(t, sp, "db1", 1, walsink.SegmentFileName(1, 8, walsink.SegmentSize))
	got, found, err := inventory.NextArchivedLSNAtOrAfter(context.Background(), sp, "db1", 1, segStartLSN(8))
	if err != nil || !found || got != segStartLSN(8) {
		t.Errorf("got=%s found=%v err=%v, want exactly seg 8's start", got, found, err)
	}
}

// TestNextArchivedLSNAtOrAfter_NothingAbove: every archived segment is
// below from — found=false, which callers treat as "fall back to the
// frontier".
func TestNextArchivedLSNAtOrAfter_NothingAbove(t *testing.T) {
	sp := newSP(t)
	putSegmentManifest(t, sp, "db1", 1, walsink.SegmentFileName(1, 2, walsink.SegmentSize))
	_, found, err := inventory.NextArchivedLSNAtOrAfter(context.Background(), sp, "db1", 1, segStartLSN(3))
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Error("found a resume point below from")
	}
}
