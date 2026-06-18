//go:build integration

package agent_test

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

func TestAgentExecutorBackupRestoreLifecycle(t *testing.T) {
	srv := testkit.StartPostgres(t)

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
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

	target := filepath.Join(t.TempDir(), "agent_restored")
	rres, err := restore.Restore(ctx, restore.Options{
		RepoURL: repoURL, Deployment: "db1", BackupID: res.BackupID, TargetDir: target, Verifier: verifier,
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if rres.FileCount == 0 {
		t.Error("agent executor: zero files after restore")
	}
	if rres.BytesWritten == 0 {
		t.Error("agent executor: zero bytes after restore")
	}

	for _, name := range []string{"PG_VERSION", "global/pg_control", "postgresql.conf"} {
		if info, err := os.Stat(filepath.Join(target, name)); err != nil || info.Size() == 0 {
			t.Errorf("agent executor: missing or empty %s", name)
		}
	}

	t.Logf("agent executor: backup %s → restore (%d files, %d bytes)", res.BackupID, rres.FileCount, rres.BytesWritten)
}
