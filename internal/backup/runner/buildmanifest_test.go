package runner

import (
	"context"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/tarsink"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/basebackup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// TestBuildManifest_StartLSNIsBaseBackupRedo pins that the manifest's
// start_lsn is PG's BASE_BACKUP start result (the checkpoint REDO point /
// backup_label START WAL LOCATION) — the LSN the WAL-retention frontier
// must protect and restore replays from — and NOT identity.XLogPos (the
// IDENTIFY_SYSTEM position taken before the backup, only a lower bound).
func TestBuildManifest_StartLSNIsBaseBackupRedo(t *testing.T) {
	sp := newSP(t)
	cas := repo.NewCAS(sp)
	sink := tarsink.New(context.Background(), cas)

	const redo = "0/5000000"    // BASE_BACKUP start = checkpoint redo
	const xlogpos = "0/4000000" // IDENTIFY_SYSTEM, earlier — a lower bound

	bb := &basebackup.Result{
		StartLSN:      redo,
		StartTimeline: 1,
		StopLSN:       "0/6000000",
		StopTimeline:  1,
	}
	identity := pg.SystemIdentity{SystemID: "7000000000000000001", XLogPos: xlogpos}

	m := buildManifest(TakeOptions{Deployment: "db1"}, bb, sink, "db1.full.x", identity, 170000)
	if m.StartLSN != redo {
		t.Errorf("StartLSN = %q, want the BASE_BACKUP redo %q (not XLogPos %q)", m.StartLSN, redo, xlogpos)
	}

	// No silent fallback: if a BASE_BACKUP result somehow carried an
	// empty start LSN, buildManifest must pass it through — NOT
	// substitute the IDENTIFY_SYSTEM lower bound, which would
	// mis-report the start and over-retain WAL. The wire parser
	// refuses empty recptrs (basebackup.parseLSNRow) and the
	// manifest gate (m.Validate) rejects an empty start_lsn before
	// commit, so an empty value never reaches a committed manifest.
	bb.StartLSN = ""
	m2 := buildManifest(TakeOptions{Deployment: "db1"}, bb, sink, "db1.full.y", identity, 170000)
	if m2.StartLSN != "" {
		t.Errorf("empty BASE_BACKUP start: StartLSN = %q, want empty (no XLogPos fallback)", m2.StartLSN)
	}
}
