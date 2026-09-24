package cli_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// Regression (M96): preflight.backup_wal_missing tells the operator
// "--skip-gap-check overrides this refusal", but a PLAIN restore (no
// --to*) builds no Recovery struct, so the flag was dropped and the
// refusal could not be overridden at all.
func TestRestore_SkipGapCheckOverridesBackupWALMissingOnPlainRestore(t *testing.T) {
	w := newReadWorld(t)
	defer w.cleanup()

	// A backup that embeds no WAL, with nothing archived: exactly the
	// shape the pre-flight refuses.
	body := []byte("data-of-b1\n")
	when := nowMinus(t, 1)
	m := &backup.Manifest{
		Schema: backup.Schema, BackupID: "b1", Deployment: "db1", Tenant: "default",
		Type: backup.BackupTypeFull, PGVersion: 17, SystemIdentifier: "7000000000000000001",
		StartLSN: "0/3000028", StopLSN: "0/30001A0", Timeline: 1,
		StartedAt: when, StoppedAt: when.Add(1),
		BackupLabel: "START WAL LOCATION: 0/3000028\n",
		Tablespaces: []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		Files: []backup.FileEntry{{Path: "PG_VERSION", Size: int64(len(body)), Mode: 0o600,
			Chunks: []backup.ChunkRef{{Hash: repo.HashOf(body), Offset: 0, Len: int64(len(body))}}}},
	}
	if _, err := repo.NewCAS(w.sp).PutChunk(context.Background(), body); err != nil {
		t.Fatal(err)
	}
	if err := w.store.Commit(context.Background(), m, w.signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}

	// Without the flag: refused as a pre-flight.
	_, errb, exit := runCLI(t, "restore", "db1", "b1", "--repo", w.repoURL,
		"--target", filepath.Join(t.TempDir(), "r1"), "--verify", "skip", "--verify-restore", "off")
	if exit != int(output.ExitPreflight) || !strings.Contains(errb, "backup_wal_missing") {
		t.Fatalf("without --skip-gap-check: exit=%d, want %d with backup_wal_missing\n%s", exit, output.ExitPreflight, errb)
	}

	// With it: the documented override works on a plain restore too.
	if _, errb, exit := runCLI(t, "restore", "db1", "b1", "--repo", w.repoURL,
		"--target", filepath.Join(t.TempDir(), "r2"), "--verify", "skip", "--verify-restore", "off",
		"--skip-gap-check"); exit != int(output.ExitOK) {
		t.Fatalf("--skip-gap-check on a plain restore: exit=%d\n%s", exit, errb)
	}
}
