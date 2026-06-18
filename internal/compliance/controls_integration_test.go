//go:build integration

package compliance_test

import (
	"context"
	"crypto/rand"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/runner"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/testkit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

func TestComplianceControlsValidation(t *testing.T) {
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

	if res.BackupID == "" {
		t.Fatal("empty backup ID — compliance control requires traceable backup identifiers")
	}
	if res.PGVersion <= 0 {
		t.Error("PG version not recorded — compliance audit requires pg_version in manifest")
	}
	if res.FileCount == 0 {
		t.Error("zero file count — compliance check requires file inventory")
	}
	if res.LogicalBytes == 0 {
		t.Error("zero logical bytes — compliance requires backup size tracking")
	}

	t.Logf("compliance: backup %s — PG %d, %d files, %d bytes — audit trail validated", res.BackupID, res.PGVersion, res.FileCount, res.LogicalBytes)
}
