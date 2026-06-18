package repo_test

import (
	"context"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// TestWALPrune_FrontierIsMinStartLSN_NotOldestStoppedAt pins the fix
// for a WAL-prune data-loss bug. The prune floor must be the MINIMUM
// start_lsn across kept backups, not the start_lsn of the
// earliest-finishing (oldest-by-StoppedAt) backup.
//
// Scenario: backup A started at an EARLY LSN but finished LATE (a
// long-running / overlapping backup); backup B started at a LATER LSN
// but finished EARLY. A WAL segment lies between their start LSNs — A
// still needs it, B does not.
//
//	A: start_lsn 0/01000000, stopped 12:00  (oldest by LSN)
//	B: start_lsn 0/05000000, stopped 10:00  (oldest by StoppedAt)
//	segment end_lsn 0/03000000  (>= A.start, < B.start)
//
// Old code keyed the frontier off StoppedAt → frontier = B.start
// (0/05000000) → the segment (end 0/03000000) looked prunable even
// though A needs it. The fix uses min(start_lsn) = A.start → the
// segment is kept.
func TestWALPrune_FrontierIsMinStartLSN_NotOldestStoppedAt(t *testing.T) {
	_, sp := newTestRepo(t)
	defer sp.Close()

	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	// A: early LSN, LATE stop.
	plantBackupManifest(t, sp, "db1", "bkpA", "0/01000000", base.Add(12*time.Hour))
	// B: later LSN, EARLY stop.
	plantBackupManifest(t, sp, "db1", "bkpB", "0/05000000", base.Add(10*time.Hour))

	// Segment that A needs (end >= A.start) but is < B.start.
	const seg = "000000010000000000000002"
	plantWALSegManifest(t, sp, "db1", 1, seg, "0/03000000", base.Add(11*time.Hour), []int64{1024})

	res, err := repo.WALPrune(context.Background(), sp, repo.WALPruneOptions{
		Deployment: "db1",
		DryRun:     true,
	})
	if err != nil {
		t.Fatalf("WALPrune: %v", err)
	}

	// Frontier must be anchored on backup A (the min-start_lsn backup).
	if res.FrontierBackupID != "bkpA" {
		t.Errorf("FrontierBackupID = %q; want bkpA (min start_lsn), not the oldest-by-StoppedAt bkpB", res.FrontierBackupID)
	}
	// The segment A still needs must NOT be a prune candidate.
	if res.SegmentsDeleted != 0 {
		t.Errorf("SegmentsDeleted = %d; want 0 — the segment is needed by backup A and must be kept", res.SegmentsDeleted)
	}
	if res.SegmentsKept != 1 {
		t.Errorf("SegmentsKept = %d; want 1", res.SegmentsKept)
	}
}
