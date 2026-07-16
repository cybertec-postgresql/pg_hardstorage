package recovery_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/recovery"
)

// putSeg plants a real segment manifest so walHighestForTimeline +
// FirstWALHoleInRange see it, mirroring the proven gapcheck_test helper.
func putSeg(t *testing.T, sp storage.StoragePlugin, deployment string, tli uint32, segNum uint64) {
	t.Helper()
	name := walsink.SegmentFileName(tli, segNum, walsink.SegmentSize)
	start := pglogrepl.LSN(segNum * uint64(walsink.SegmentSize))
	m := &walsink.SegmentManifest{
		Schema:           walsink.Schema,
		Deployment:       deployment,
		SystemIdentifier: "7000000000000000001",
		Timeline:         tli,
		SegmentNumber:    segNum,
		SegmentName:      name,
		StartLSN:         start.String(),
		EndLSN:           (start + pglogrepl.LSN(walsink.SegmentSize)).String(),
		SegmentSize:      walsink.SegmentSize,
	}
	raw, err := m.MarshalToBytes()
	if err != nil {
		t.Fatal(err)
	}
	key := walsink.SegmentPath(deployment, tli, name)
	if _, err := sp.Put(context.Background(), key, bytes.NewReader(raw),
		storage.PutOptions{ContentLength: int64(len(raw))}); err != nil {
		t.Fatal(err)
	}
}

// Regression (#4): recovery windows must CAP LatestRestoreLSN at the
// first WAL archive hole rather than advertising a PITR range straight
// across it. The backup stops in segment 3 (StopLSN 0/30001A0); we
// archive segments 3, 4, 6 — segment 5 is MISSING — so replay HALTS at
// segment 5's start (0/5000000), which is where the window must end.
func TestWindows_CapsAtArchiveHole(t *testing.T) {
	w := setupWorld(t)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	w.commitBackup(t, "db1", now.Add(-1*time.Hour), 1<<30, false, backup.BackupTypeFull, 1)

	for _, seg := range []uint64{3, 4, 6} { // hole at segment 5
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
	const holeStart = "0/5000000" // segment 5 start
	if win.LatestRestoreLSN != holeStart {
		t.Errorf("LatestRestoreLSN = %q, want %q (capped at the hole, NOT the highest segment)",
			win.LatestRestoreLSN, holeStart)
	}
	foundGap := false
	for _, g := range win.Gaps {
		if g.Source == "archive_scan" && g.StartLSN == holeStart {
			foundGap = true
		}
	}
	if !foundGap {
		t.Errorf("expected an archive_scan gap at %s; gaps=%+v", holeStart, win.Gaps)
	}
}
