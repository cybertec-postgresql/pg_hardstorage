package cli

// wal_prestream_gap_test.go — the fresh-slot stream start records the
// window no one will ever archive.
//
// See wal_prestream_gap.go for the full story; the short form: `init
// --quick` then `wal stream` leaves [backup.stop, streamStart) covered
// by nothing, PG cannot tell that hole from the end of the archive at
// recovery, and the chaos gate's first boot-proof caught a backup
// promoting cleanly without the entire fault-window workload. The
// record is what lets the restore preflight refuse instead.

import (
	"context"
	"crypto/rand"
	"testing"
	"time"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/gapstate"
)

func prestreamWorld(t *testing.T, stopLSN string) (opts walStreamOptions, deps struct {
	sp    interface{ Close() error }
	spp   storage.StoragePlugin
	run   func(startLSN pglogrepl.LSN) []gapstate.Record
	runOn func(tli uint32, startLSN pglogrepl.LSN) []gapstate.Record
}) {
	t.Helper()
	spp, _ := newFsRepo(t)
	priv, _, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := backup.LoadSigner(priv)
	if err != nil {
		t.Fatal(err)
	}
	if stopLSN != "" {
		m := &backup.Manifest{
			Schema: backup.Schema, BackupID: "b1", Deployment: "db1", Tenant: "default",
			Type: backup.BackupTypeFull, PGVersion: 17,
			SystemIdentifier: "7000000000000000001",
			StartLSN:         "0/3000028", StopLSN: stopLSN, Timeline: 1,
			StartedAt: time.Now().UTC().Add(-time.Hour), StoppedAt: time.Now().UTC().Add(-59 * time.Minute),
			BackupLabel: "START WAL LOCATION: 0/3000028\n",
			Tablespaces: []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
			Files: []backup.FileEntry{{Path: "PG_VERSION", Size: 3, Mode: 0o600,
				Chunks: []backup.ChunkRef{{Hash: repo.HashOf([]byte("17\n")), Offset: 0, Len: 3}}}},
		}
		if _, err := repo.NewCAS(spp).PutChunk(context.Background(), []byte("17\n")); err != nil {
			t.Fatal(err)
		}
		if err := backup.NewManifestStore(spp).Commit(context.Background(), m, signer, backup.CommitOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	o := walStreamOptions{deployment: "db1", slotName: "s1"}
	deps.spp = spp
	deps.runOn = func(tli uint32, startLSN pglogrepl.LSN) []gapstate.Record {
		d, _ := captureDispatcher(t)
		recordPreStreamGap(context.Background(), d, spp, o, tli, startLSN)
		recs, lerr := gapstate.New(spp).List(context.Background(), "db1")
		if lerr != nil {
			t.Fatal(lerr)
		}
		return recs
	}
	deps.run = func(startLSN pglogrepl.LSN) []gapstate.Record {
		return deps.runOn(1, startLSN)
	}
	return o, deps
}

// TestRecordPreStreamGap_RecordsTheUncoveredWindow is the fix.
func TestRecordPreStreamGap_RecordsTheUncoveredWindow(t *testing.T) {
	_, deps := prestreamWorld(t, "0/30001A0")
	recs := deps.run(pglogrepl.LSN(0x6000000))
	if len(recs) != 1 {
		t.Fatalf("want 1 gap record, got %d — without it a --to-latest restore from b1 "+
			"silently truncates at the window and promotes", len(recs))
	}
	if recs[0].GapStartLSN != "0/30001A0" || recs[0].GapEndLSN != "0/6000000" {
		t.Errorf("recorded [%s, %s), want [0/30001A0, 0/6000000)",
			recs[0].GapStartLSN, recs[0].GapEndLSN)
	}
}

// TestRecordPreStreamGap_IdempotentAcrossRestarts: a crash-loop before
// the first segment commits re-enters the fresh-slot path every time;
// it must not pile up duplicate records.
func TestRecordPreStreamGap_IdempotentAcrossRestarts(t *testing.T) {
	_, deps := prestreamWorld(t, "0/30001A0")
	deps.run(pglogrepl.LSN(0x6000000))
	recs := deps.run(pglogrepl.LSN(0x6000000))
	if len(recs) != 1 {
		t.Fatalf("want 1 record after a repeat start, got %d", len(recs))
	}
}

// TestRecordPreStreamGap_NoBackups_NoRecord: a genuinely fresh
// deployment (stream-first, backup later) has nothing uncovered.
func TestRecordPreStreamGap_NoBackups_NoRecord(t *testing.T) {
	_, deps := prestreamWorld(t, "")
	if recs := deps.run(pglogrepl.LSN(0x6000000)); len(recs) != 0 {
		t.Fatalf("recorded a gap with no backups at all: %+v", recs)
	}
}

// TestRecordPreStreamGap_StreamStartsBelowStop_NoRecord: when the
// slot anchors at or before the backup's stop (WAL retention held),
// nothing is uncovered and recording would be a false alarm on every
// healthy first start.
func TestRecordPreStreamGap_StreamStartsBelowStop_NoRecord(t *testing.T) {
	_, deps := prestreamWorld(t, "0/30001A0")
	if recs := deps.run(pglogrepl.LSN(0x3000000)); len(recs) != 0 {
		t.Fatalf("recorded a gap although the stream starts below the backup's stop: %+v", recs)
	}
}

// TestRecordPreStreamGap_FrontierBoundsTheWindow is the Patroni
// failover shape (bug #20): the deployment has streamed for months —
// archived segments reach 0/7000000 — a failover destroys the slot on
// the new leader, and the reconnect creates a FRESH slot at 0/9000000.
// The uncovered window is [frontier, start), NOT [oldest-backup.stop,
// start): claiming already-archived WAL as a gap would make every
// unbounded restore from every older backup refuse forever (gap
// records are eternal), which trains operators to --skip-gap-check
// past the refusals that are true.
func TestRecordPreStreamGap_FrontierBoundsTheWindow(t *testing.T) {
	_, deps := prestreamWorld(t, "0/30001A0")
	for seg := uint64(3); seg <= 6; seg++ {
		plantWALSeg(t, deps.spp, "db1", 1, seg)
	}
	recs := deps.run(pglogrepl.LSN(0x9000000))
	if len(recs) != 1 {
		t.Fatalf("want 1 gap record, got %d", len(recs))
	}
	if recs[0].GapStartLSN != "0/7000000" || recs[0].GapEndLSN != "0/9000000" {
		t.Errorf("recorded [%s, %s), want [0/7000000, 0/9000000) — the WAL below the "+
			"frontier IS archived; recording it as missing is a permanent false refusal "+
			"for every backup older than the failover",
			recs[0].GapStartLSN, recs[0].GapEndLSN)
	}
}

// TestRecordPreStreamGap_FrontierOnPriorTimeline: same failover shape,
// but the fresh slot is on the NEW timeline (the normal Patroni case —
// the promoted leader reports TLI 2, everything archived so far is on
// TLI 1). The frontier lookup must look one timeline down, exactly
// like the coordinator's (nearest below, never max-across — diverged
// old-timeline WAL past the branch must not count).
func TestRecordPreStreamGap_FrontierOnPriorTimeline(t *testing.T) {
	_, deps := prestreamWorld(t, "0/30001A0")
	for seg := uint64(3); seg <= 6; seg++ {
		plantWALSeg(t, deps.spp, "db1", 1, seg)
	}
	recs := deps.runOn(2, pglogrepl.LSN(0x9000000))
	if len(recs) != 1 {
		t.Fatalf("want 1 gap record, got %d", len(recs))
	}
	if recs[0].GapStartLSN != "0/7000000" || recs[0].GapEndLSN != "0/9000000" {
		t.Errorf("recorded [%s, %s), want [0/7000000, 0/9000000)",
			recs[0].GapStartLSN, recs[0].GapEndLSN)
	}
}

// TestRecordPreStreamGap_FrontierCoversStart_NoRecord: the archive
// already reaches the fresh slot's anchor (quick slot recreation while
// archiving was current) — nothing is uncovered, and a record here
// would be a false alarm on every healthy slot rebuild.
func TestRecordPreStreamGap_FrontierCoversStart_NoRecord(t *testing.T) {
	_, deps := prestreamWorld(t, "0/30001A0")
	for seg := uint64(3); seg <= 9; seg++ {
		plantWALSeg(t, deps.spp, "db1", 1, seg)
	}
	if recs := deps.run(pglogrepl.LSN(0x9000000)); len(recs) != 0 {
		t.Fatalf("recorded a gap although the archive covers the stream start: %+v", recs)
	}
}
