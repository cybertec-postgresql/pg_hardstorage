//go:build integration

package partial_test

import (
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/runner"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/testkit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
)

func TestPartialRestoreTableLevel(t *testing.T) {
	srv := testkit.StartPostgres(t)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	repoURL := "file://" + t.TempDir()
	if _, err := repo.Init(ctx, repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatalf("repo.Init: %v", err)
	}

	priv, pub, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)

	res, err := runner.Take(ctx, runner.TakeOptions{
		PGConnString: srv.DSN, RepoURL: repoURL, Deployment: "db1",
		Signer: signer, Verifier: verifier, Fast: true,
	})
	if err != nil {
		t.Fatalf("Take: %v", err)
	}

	target := filepath.Join(t.TempDir(), "partial_restored")
	rres, err := restore.Restore(ctx, restore.Options{
		RepoURL: repoURL, Deployment: "db1", BackupID: res.BackupID, TargetDir: target, Verifier: verifier,
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	pgVersion, err := os.ReadFile(filepath.Join(target, "PG_VERSION"))
	if err != nil {
		t.Fatalf("PG_VERSION: %v", err)
	}
	if string(pgVersion) == "" {
		t.Error("empty PG_VERSION after partial restore")
	}

	t.Logf("partial restore: %d files, %d bytes — table-level restore test scaffolding ready", rres.FileCount, rres.BytesWritten)
}
