package recovery_test

import (
	"context"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/recovery"
)

// Regression (M102): a CONTIGUOUS archive must not report an archive
// hole at the frontier. HighestArchivedLSN returns the exclusive end
// of the newest segment; passing it as the inclusive upper bound of
// FirstWALHoleInRange asked whether the not-yet-existing next segment
// was archived, so every backup's window carried a bogus zero-width
// archive_scan gap at the frontier and counted as "with gaps".
func TestWindows_ContiguousArchiveHasNoFrontierHole(t *testing.T) {
	w := setupWorld(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	w.commitBackup(t, "db1", now.Add(-1*time.Hour), 1<<30, false, backup.BackupTypeFull, 1)

	for _, seg := range []uint64{3, 4, 5} { // contiguous, no hole
		putSeg(t, w.sp, "db1", 1, seg)
	}

	r, err := recovery.Windows(context.Background(), w.sp, "db1", recovery.WindowsOptions{
		Verifier: w.verifier,
		Now:      now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Windows) != 1 {
		t.Fatalf("want 1 window, got %d", len(r.Windows))
	}
	win := r.Windows[0]
	if len(win.Gaps) != 0 {
		t.Errorf("contiguous archive reported gaps: %+v", win.Gaps)
	}
	if win.LatestRestoreLSN != "0/6000000" {
		t.Errorf("LatestRestoreLSN = %q, want 0/6000000 (the archive frontier)", win.LatestRestoreLSN)
	}
	if r.Coverage.WindowsWithGaps != 0 {
		t.Errorf("WindowsWithGaps = %d, want 0", r.Coverage.WindowsWithGaps)
	}
}
