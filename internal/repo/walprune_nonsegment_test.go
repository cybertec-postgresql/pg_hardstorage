package repo_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// TestWALPrune_GapRecordsAreNotSegments: wal/<dep>/gaps/*.json are gap
// records, not segment manifests. listWALSegmentKeys used to admit any
// .json under wal/<dep>/ except timelines/, so every gap record was read
// as a segment, had no end_lsn, and was counted in SegmentsFailed — a
// permanent, unfixable "failure" on every prune of a deployment that has
// ever recorded a gap, which (with a non-zero exit on failures) would
// make `wal prune --apply` fail forever. And had a record ever decoded
// with an end_lsn, prune would have DELETED it, erasing the evidence
// restore uses to refuse a PITR into a hole.
func TestWALPrune_GapRecordsAreNotSegments(t *testing.T) {
	_, sp := newTestRepo(t)
	defer sp.Close()
	plantBackupManifest(t, sp, "db1", "db1.full.aaa",
		"0/05000000", time.Now().Add(-1*time.Hour))
	plantWALSegManifest(t, sp, "db1", 1, "000000010000000000000001",
		"0/02000000", time.Now().Add(-3*time.Hour), []int64{1024})

	gapKey := "wal/db1/gaps/00000001-1700000000000000000-abcd.json"
	gap := []byte(`{"schema":"pg_hardstorage.wal_gap.v1","deployment":"db1","timeline":1,` +
		`"gap_start_lsn":"0/1000000","gap_end_lsn":"0/1800000"}`)
	if _, err := sp.Put(context.Background(), gapKey, bytes.NewReader(gap),
		storage.PutOptions{ContentLength: int64(len(gap))}); err != nil {
		t.Fatal(err)
	}

	res, err := repo.WALPrune(context.Background(), sp, repo.WALPruneOptions{Deployment: "db1"})
	if err != nil {
		t.Fatalf("WALPrune: %v", err)
	}
	if res.SegmentsFailed != 0 {
		t.Errorf("SegmentsFailed = %d (%v); a gap record is not a segment and must not be "+
			"read as one", res.SegmentsFailed, res.Failures)
	}
	if res.SegmentsConsidered != 1 {
		t.Errorf("SegmentsConsidered = %d, want 1 (the one real segment)", res.SegmentsConsidered)
	}
	if _, err := sp.Stat(context.Background(), gapKey); err != nil {
		t.Errorf("gap record disturbed: %v", err)
	}
}
