package restore

// gapcheck_standby_test.go — a standby over a holed archive must be
// warned about, not left to discover it.
//
// preflightWALContiguity used to run only for LSN targets. A standby has
// no target by construction — WriteRecoveryFiles rejects one in standby
// mode — so it was never checked, and a standby is the consumer that
// suffers most from a hole.
//
// The reason it is the worst case is how PG behaves when restore_command
// cannot supply a segment. For a one-shot PITR that is an error and
// recovery stops loudly. For a STANDBY it is the normal signal for "that
// WAL has not been archived yet, wait for it" — so the instance stays up,
// keeps answering read queries, keeps reporting healthy, and is frozen at
// the gap. Nothing distinguishes it from a standby that is simply
// caught up. An operator holding it for DR finds out at failover.
//
// Failovers are how these holes appear in the first place, which is what
// makes this worth checking rather than assuming.

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
)

const gapTestDeployment = "db1"

// gapTestRepo opens an empty file-backed repo.
func gapTestRepo(t *testing.T) storage.StoragePlugin {
	t.Helper()
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{
		URL: &url.URL{Scheme: "file", Path: t.TempDir()},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	return sp
}

// plantSeg writes a committed segment manifest for (tli, segNum).
func plantSeg(t *testing.T, sp storage.StoragePlugin, tli uint32, segNum uint64) {
	t.Helper()
	name := walsink.SegmentFileName(tli, segNum, walsink.SegmentSize)
	start := pglogrepl.LSN(segNum * uint64(walsink.SegmentSize))
	m := &walsink.SegmentManifest{
		Schema:           walsink.Schema,
		Deployment:       gapTestDeployment,
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
	key := fmt.Sprintf("wal/%s/%08X/%s.json", gapTestDeployment, tli, name)
	if _, err := sp.Put(context.Background(), key, bytes.NewReader(raw),
		storage.PutOptions{ContentLength: int64(len(raw))}); err != nil {
		t.Fatal(err)
	}
}

// standbyRecovery is the shape standby.Create builds: recovery on, no
// target of any kind, follow the newest timeline.
func standbyRecovery() *Recovery {
	return &Recovery{
		Enable:      true,
		StandbyMode: true,
		Timeline:    "latest",
	}
}

func collectWarnings(t *testing.T) (func(*output.Event), *[]string) {
	t.Helper()
	var got []string
	return func(ev *output.Event) {
		if ev != nil {
			got = append(got, ev.Component+"."+ev.Op)
		}
	}, &got
}

// TestPreflightWALContiguity_StandbyOverAHole is the regression test.
//
// The backup stops inside segment 2; the archive holds 2, 3, then jumps
// to 9. A standby built on this replays to the end of segment 3 and
// stops there, permanently and quietly.
func TestPreflightWALContiguity_StandbyOverAHole(t *testing.T) {
	sp := gapTestRepo(t)
	for _, n := range []uint64{2, 3, 9, 10} {
		plantSeg(t, sp, 1, n)
	}
	m := &backup.Manifest{
		Timeline: 1,
		StopLSN:  pglogrepl.LSN(2 * uint64(walsink.SegmentSize)).String(),
	}

	emit, got := collectWarnings(t)
	preflightWALContiguity(context.Background(), sp, gapTestDeployment, m, standbyRecovery(), emit)

	if len(*got) == 0 {
		t.Fatalf("no warning for a standby built over a hole in the archive.\n\n" +
			"Segments 4..8 are missing, so the standby replays to the end of segment 3 and " +
			"waits. It does not fail: a standby treats an unavailable segment as \"not " +
			"archived yet\", so PG stays up, answers read queries, and reports healthy while " +
			"frozen. The check was skipped because a standby has no target LSN — but its " +
			"upper bound is knowable, it is simply the archive frontier.")
	}
	joined := strings.Join(*got, "\n")
	if !strings.Contains(joined, "wal_archive_hole") {
		t.Errorf("warning is not wal_archive_hole:\n%s", joined)
	}
}

// TestPreflightWALContiguity_StandbyOverAContiguousArchive: no hole, no
// warning. This runs on every standby creation, so a false positive
// would teach operators to ignore the one that matters.
func TestPreflightWALContiguity_StandbyOverAContiguousArchive(t *testing.T) {
	sp := gapTestRepo(t)
	for n := uint64(2); n <= 10; n++ {
		plantSeg(t, sp, 1, n)
	}
	m := &backup.Manifest{
		Timeline: 1,
		StopLSN:  pglogrepl.LSN(2 * uint64(walsink.SegmentSize)).String(),
	}

	emit, got := collectWarnings(t)
	preflightWALContiguity(context.Background(), sp, gapTestDeployment, m, standbyRecovery(), emit)
	if len(*got) > 0 {
		t.Errorf("warned about a contiguous archive:\n%s", strings.Join(*got, "\n"))
	}
}

// TestPreflightWALContiguity_FreshBackupNotYetArchived: immediately
// after a backup the frontier can sit below the backup's stop LSN,
// because the closing WAL has not been archived yet. That is normal and
// must not warn — a standby created right after a backup is the most
// ordinary case there is.
func TestPreflightWALContiguity_FreshBackupNotYetArchived(t *testing.T) {
	sp := gapTestRepo(t)
	for _, n := range []uint64{1, 2} {
		plantSeg(t, sp, 1, n)
	}
	m := &backup.Manifest{
		Timeline: 1,
		StopLSN:  pglogrepl.LSN(7 * uint64(walsink.SegmentSize)).String(),
	}

	emit, got := collectWarnings(t)
	preflightWALContiguity(context.Background(), sp, gapTestDeployment, m, standbyRecovery(), emit)
	if len(*got) > 0 {
		t.Errorf("warned when the archive simply had not caught up to the backup yet:\n%s",
			strings.Join(*got, "\n"))
	}
}

// TestPreflightWALContiguity_TimeTargetStillSkipped pins the part of the
// old limitation that is real. Only PG can say which LSN a timestamp
// resolves to, so there is no range to scan and guessing one would
// produce warnings nobody can act on.
func TestPreflightWALContiguity_TimeTargetStillSkipped(t *testing.T) {
	sp := gapTestRepo(t)
	for _, n := range []uint64{2, 3, 9} {
		plantSeg(t, sp, 1, n)
	}
	m := &backup.Manifest{
		Timeline: 1,
		StopLSN:  pglogrepl.LSN(2 * uint64(walsink.SegmentSize)).String(),
	}
	rec := &Recovery{Enable: true, Timeline: "latest", TargetTime: time.Now()}

	emit, got := collectWarnings(t)
	preflightWALContiguity(context.Background(), sp, gapTestDeployment, m, rec, emit)
	if len(*got) > 0 {
		t.Errorf("warned on a TIME target, whose LSN only PG can resolve:\n%s",
			strings.Join(*got, "\n"))
	}
}

// TestPreflightWALContiguity_SkipGapCheckStillHonoured: the operator's
// explicit override must keep working for the new path too.
func TestPreflightWALContiguity_SkipGapCheckStillHonoured(t *testing.T) {
	sp := gapTestRepo(t)
	for _, n := range []uint64{2, 3, 9} {
		plantSeg(t, sp, 1, n)
	}
	m := &backup.Manifest{
		Timeline: 1,
		StopLSN:  pglogrepl.LSN(2 * uint64(walsink.SegmentSize)).String(),
	}
	rec := standbyRecovery()
	rec.SkipGapCheck = true

	emit, got := collectWarnings(t)
	preflightWALContiguity(context.Background(), sp, gapTestDeployment, m, rec, emit)
	if len(*got) > 0 {
		t.Errorf("--skip-gap-check did not suppress the warning:\n%s", strings.Join(*got, "\n"))
	}
}
