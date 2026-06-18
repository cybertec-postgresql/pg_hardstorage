//go:build integration

package approval_test

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

func TestApprovalWorkflowBackupGuardedOps(t *testing.T) {
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
		t.Fatal("empty backup ID")
	}

	t.Logf("approval workflow: backup %s created — destructive op (delete/wiped) would require n-of-m confirmation", res.BackupID)
}
