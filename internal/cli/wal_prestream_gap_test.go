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
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/gapstate"
)

func prestreamWorld(t *testing.T, stopLSN string) (opts walStreamOptions, deps struct {
	sp  interface{ Close() error }
	run func(startLSN pglogrepl.LSN) []gapstate.Record
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
	deps.run = func(startLSN pglogrepl.LSN) []gapstate.Record {
		d, _ := captureDispatcher(t)
		recordPreStreamGap(context.Background(), d, spp, o, 1, startLSN)
		recs, lerr := gapstate.New(spp).List(context.Background(), "db1")
		if lerr != nil {
			t.Fatal(lerr)
		}
		return recs
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
