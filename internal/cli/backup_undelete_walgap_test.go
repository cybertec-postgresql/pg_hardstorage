package cli

// backup_undelete_walgap_test.go — resurrection must re-check the WAL,
// not just the chunks. See backup_undelete_walgap.go for the scenario:
// `wal prune` legitimately deletes the archived window after a
// TOMBSTONED backup's stop (dead backups don't hold the frontier), and
// before this check `backup undelete` handed back a backup whose
// --to-latest silently truncated at the pruned hole — no gap record,
// so none of the restore-side refusals could fire.

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/gapstate"
)

// plantWALSeg commits one archived-segment manifest for (tli, segNum),
// the same fixture shape the restore-side gap tests use.
func plantWALSeg(t *testing.T, sp storage.StoragePlugin, deployment string, tli uint32, segNum uint64) {
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
		CreatedAt:        time.Now().UTC(),
	}
	raw, err := m.MarshalToBytes()
	if err != nil {
		t.Fatal(err)
	}
	key := fmt.Sprintf("wal/%s/%08X/%s.json", deployment, tli, name)
	if _, err := sp.Put(context.Background(), key, bytes.NewReader(raw),
		storage.PutOptions{ContentLength: int64(len(raw))}); err != nil {
		t.Fatal(err)
	}
}

func resurrectedManifest(stopLSN string) *backup.Manifest {
	return &backup.Manifest{
		Schema: backup.Schema, BackupID: "b1", Deployment: "db1",
		StopLSN: stopLSN, Timeline: 1,
	}
}

// TestRecordResurrectedWALGap_PrunedWindow_Records is the finding: the
// backup stops in segment 3, the janitors kept only segments 8-9, and
// the resurrection must persist [seg3.start, seg8.start) so the
// restore preflights refuse recovery across the pruned window.
func TestRecordResurrectedWALGap_PrunedWindow_Records(t *testing.T) {
	sp, _ := newFsRepo(t)
	plantWALSeg(t, sp, "db1", 1, 8)
	plantWALSeg(t, sp, "db1", 1, 9)
	d, buf := captureDispatcher(t)

	got := recordResurrectedWALGap(context.Background(), d, sp, "db1", "b1",
		resurrectedManifest("0/3000000"))
	if got != "0/3000000..0/8000000" {
		t.Fatalf("recorded window = %q, want 0/3000000..0/8000000\n\n"+
			"Without the record, the resurrected backup's --to-latest replays its bundled "+
			"WAL, hits the pruned hole, and PG cannot tell it from the end of the archive: "+
			"a one-shot restore PROMOTES silently behind; a standby freezes forever.", got)
	}
	recs, err := gapstate.New(sp).List(context.Background(), "db1")
	if err != nil || len(recs) != 1 {
		t.Fatalf("gap records = %d (err=%v), want exactly 1", len(recs), err)
	}
	r := recs[0]
	if r.GapStartLSN != "0/3000000" || r.GapEndLSN != "0/8000000" || r.Timeline != 1 ||
		r.SlotName != "backup-undelete" {
		t.Errorf("record fields wrong: %+v", r)
	}
	if !strings.Contains(buf.String(), "resurrected_wal_gap") {
		t.Errorf("no resurrected_wal_gap event emitted:\n%s", buf.String())
	}
}

// TestRecordResurrectedWALGap_CoverageIntact_NoRecord: the janitors
// kept this backup's forward window — recording (and later refusing)
// would be the false positive that gets the check bypassed.
func TestRecordResurrectedWALGap_CoverageIntact_NoRecord(t *testing.T) {
	sp, _ := newFsRepo(t)
	for seg := uint64(3); seg <= 9; seg++ {
		plantWALSeg(t, sp, "db1", 1, seg)
	}
	d, buf := captureDispatcher(t)
	if got := recordResurrectedWALGap(context.Background(), d, sp, "db1", "b1",
		resurrectedManifest("0/3000000")); got != "" {
		t.Fatalf("recorded %q over intact coverage", got)
	}
	if recs, _ := gapstate.New(sp).List(context.Background(), "db1"); len(recs) != 0 {
		t.Fatalf("gap record persisted over intact coverage: %+v", recs)
	}
	if strings.Contains(buf.String(), "resurrected_wal_gap") {
		t.Errorf("event emitted for intact coverage:\n%s", buf.String())
	}
}

// TestRecordResurrectedWALGap_EmptyArchive_NoRecord: no archived WAL
// past the stop means recovery honestly ends at the archive's end —
// there is no hole to describe, and target-reachability owns the
// "archive is empty" complaint.
func TestRecordResurrectedWALGap_EmptyArchive_NoRecord(t *testing.T) {
	sp, _ := newFsRepo(t)
	d, _ := captureDispatcher(t)
	if got := recordResurrectedWALGap(context.Background(), d, sp, "db1", "b1",
		resurrectedManifest("0/3000000")); got != "" {
		t.Fatalf("recorded %q with an empty archive", got)
	}
}

// TestRecordResurrectedWALGap_Idempotent: a repeated
// tombstone/resurrect cycle (or a re-run undelete) must not pile up
// duplicate records for the same window.
func TestRecordResurrectedWALGap_Idempotent(t *testing.T) {
	sp, _ := newFsRepo(t)
	plantWALSeg(t, sp, "db1", 1, 8)
	d, _ := captureDispatcher(t)
	m := resurrectedManifest("0/3000000")
	first := recordResurrectedWALGap(context.Background(), d, sp, "db1", "b1", m)
	second := recordResurrectedWALGap(context.Background(), d, sp, "db1", "b1", m)
	if first == "" || first != second {
		t.Fatalf("windows differ across runs: %q vs %q", first, second)
	}
	if recs, _ := gapstate.New(sp).List(context.Background(), "db1"); len(recs) != 1 {
		t.Fatalf("duplicate records: %d, want 1", len(recs))
	}
}
