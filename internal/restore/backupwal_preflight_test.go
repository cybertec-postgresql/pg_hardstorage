package restore

import (
	"context"
	"errors"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// A backup with no embedded WAL and nothing archived can never become
// consistent: recovery waits forever for the next segment. This was
// found by running the first-backup journey end to end — the restore
// "succeeded", then the boot check timed out after minutes. It must be
// refused up front, and ONLY in that case.

func manifestWith(paths ...string) *backup.Manifest {
	m := &backup.Manifest{BackupID: "db1.full.x", Timeline: 1, StopLSN: "0/3000120"}
	for _, p := range paths {
		m.Files = append(m.Files, backup.FileEntry{Path: p})
	}
	return m
}

func TestBackupWALPreflight(t *testing.T) {
	ctx := context.Background()
	sp := gapTestRepo(t)
	dep := gapTestDeployment

	// Nothing embedded, nothing archived: refuse, as a pre-flight (exit 4).
	err := preflightBackupWALAvailable(ctx, sp, dep, manifestWith("base/1/1259", "global/pg_control"), nil)
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != "preflight.backup_wal_missing" {
		t.Fatalf("want preflight.backup_wal_missing, got %v", err)
	}
	if output.ExitCodeFor(err) != output.ExitPreflight {
		t.Errorf("exit %d, want %d (pre-flight refusal)", output.ExitCodeFor(err), output.ExitPreflight)
	}

	// The backup embeds its WAL (--include-wal): fine.
	if err := preflightBackupWALAvailable(ctx, sp, dep,
		manifestWith("base/1/1259", "pg_wal/000000010000000000000003"), nil); err != nil {
		t.Errorf("a self-contained backup must not be refused: %v", err)
	}
	// pg_wal/archive_status is not WAL.
	if err := preflightBackupWALAvailable(ctx, sp, dep,
		manifestWith("pg_wal/archive_status/x.done"), nil); err == nil {
		t.Error("archive_status entries are not embedded WAL")
	}

	// The operator's override is honoured.
	if err := preflightBackupWALAvailable(ctx, sp, dep, manifestWith(),
		&Recovery{Enable: true, SkipGapCheck: true}); err != nil {
		t.Errorf("--skip-gap-check must bypass the refusal: %v", err)
	}

	// Archiving is happening for this deployment/timeline: not our call.
	plantSeg(t, sp, 1, 3)
	if err := preflightBackupWALAvailable(ctx, sp, dep, manifestWith(), nil); err != nil {
		t.Errorf("with WAL archived, the ordinary gap checks judge the details; got %v", err)
	}
}
