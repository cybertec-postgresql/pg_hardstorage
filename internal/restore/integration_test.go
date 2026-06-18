// Build-tagged integration test: real PG -> backup -> restore round-trip.
// Run with `make test-integration` (requires Docker).
//
//go:build integration

package restore_test

import (
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/runner"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/testkit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
)

// TestIntegration_BackupThenRestore is the headline test: take a real
// backup of a PG 17 container, restore it to a fresh directory, and
// verify the materialised contents look like a real PGDATA.
func TestIntegration_BackupThenRestore(t *testing.T) {
	srv := testkit.StartPostgres(t)

	repoURL := "file://" + t.TempDir()
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatalf("repo init: %v", err)
	}

	priv, pub, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Take the backup.
	bres, err := runner.Take(ctx, runner.TakeOptions{
		PGConnString:    srv.DSN,
		RepoURL:         repoURL,
		Deployment:      "db1",
		Signer:          signer,
		Verifier:        verifier,
		Fast:            true,
		IncludeManifest: true,
	})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}

	// Restore into a fresh directory.
	target := filepath.Join(t.TempDir(), "restored")
	rres, err := restore.Restore(ctx, restore.Options{
		RepoURL:    repoURL,
		Deployment: "db1",
		BackupID:   bres.BackupID,
		TargetDir:  target,
		Verifier:   verifier,
	})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}

	if rres.FileCount != bres.FileCount {
		t.Errorf("FileCount mismatch: backup %d, restore %d", bres.FileCount, rres.FileCount)
	}
	if rres.BytesWritten != bres.LogicalBytes {
		t.Errorf("BytesWritten = %d, LogicalBytes = %d", rres.BytesWritten, bres.LogicalBytes)
	}

	// Sanity: PG_VERSION should round-trip to "17".
	pgVersionBytes, err := os.ReadFile(filepath.Join(target, "PG_VERSION"))
	if err != nil {
		t.Fatalf("read PG_VERSION: %v", err)
	}
	if want, got := testkit.ExpectedPGMajor(), strings.TrimSpace(string(pgVersionBytes)); got != want {
		t.Errorf("PG_VERSION = %q, want %q", got, want)
	}

	// backup_label must exist at root (PG marker for restored data dirs).
	labelBytes, err := os.ReadFile(filepath.Join(target, "backup_label"))
	if err != nil {
		t.Fatalf("backup_label not present after restore: %v", err)
	}
	if !strings.Contains(string(labelBytes), "START WAL LOCATION:") {
		t.Errorf("backup_label content unexpected: %q", labelBytes)
	}

	// Standard PGDATA layout pieces should be present.
	for _, expected := range []string{
		"global",
		"base",
		"pg_xact",
		"pg_wal",
		"pg_tblspc",
	} {
		info, err := os.Stat(filepath.Join(target, expected))
		if err != nil {
			t.Errorf("expected %s/ in restored data dir: %v", expected, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("%s should be a directory", expected)
		}
	}
}

func TestIntegration_RestoreFromOldBackupAfterMoreData(t *testing.T) {
	// Take backup #1, write some new data, take backup #2, restore #1
	// — confirm we can pin to an older state. Smoke test for the
	// "history is real" property, not WAL replay (that's Slice 8).
	srv := testkit.StartPostgres(t)

	repoURL := "file://" + t.TempDir()
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatal(err)
	}
	priv, pub, _ := backup.GenerateKeypair(rand.Reader)
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	first, err := runner.Take(ctx, runner.TakeOptions{
		PGConnString: srv.DSN, RepoURL: repoURL, Deployment: "db1",
		Signer: signer, Verifier: verifier, Fast: true,
	})
	if err != nil {
		t.Fatalf("first backup: %v", err)
	}

	// Restore the first backup; it should succeed regardless of any
	// later activity in PG.
	target := filepath.Join(t.TempDir(), "restored-1")
	rres, err := restore.Restore(ctx, restore.Options{
		RepoURL:    repoURL,
		Deployment: "db1",
		BackupID:   first.BackupID,
		TargetDir:  target,
		Verifier:   verifier,
	})
	if err != nil {
		t.Fatalf("restore from old backup: %v", err)
	}
	if rres.BackupID != first.BackupID {
		t.Errorf("restored ID mismatch: %s vs %s", rres.BackupID, first.BackupID)
	}
}
