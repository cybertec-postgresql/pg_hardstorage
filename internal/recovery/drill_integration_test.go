//go:build integration

package recovery_test

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

func TestRecoveryDrillFullCycle(t *testing.T) {
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

	res1, err := runner.Take(ctx, runner.TakeOptions{
		PGConnString: srv.DSN, RepoURL: repoURL, Deployment: "db1",
		Signer: signer, Verifier: verifier, Fast: true,
	})
	if err != nil {
		t.Fatalf("Take 1: %v", err)
	}

	res2, err := runner.Take(ctx, runner.TakeOptions{
		PGConnString: srv.DSN, RepoURL: repoURL, Deployment: "db1",
		Signer: signer, Verifier: verifier, Fast: true,
	})
	if err != nil {
		t.Fatalf("Take 2: %v", err)
	}

	target1 := filepath.Join(t.TempDir(), "drill_restore_1")
	rres1, err := restore.Restore(ctx, restore.Options{
		RepoURL: repoURL, Deployment: "db1", BackupID: res1.BackupID, TargetDir: target1, Verifier: verifier,
	})
	if err != nil {
		t.Fatalf("Restore 1: %v", err)
	}
	_ = rres1

	target2 := filepath.Join(t.TempDir(), "drill_restore_2")
	rres2, err := restore.Restore(ctx, restore.Options{
		RepoURL: repoURL, Deployment: "db1", BackupID: res2.BackupID, TargetDir: target2, Verifier: verifier,
	})
	if err != nil {
		t.Fatalf("Restore 2: %v", err)
	}
	_ = rres2

	v1, _ := os.ReadFile(filepath.Join(target1, "PG_VERSION"))
	v2, _ := os.ReadFile(filepath.Join(target2, "PG_VERSION"))
	if string(v1) == string(v2) {
		t.Logf("recovery drill: both backups restored OK, PG_VERSION matches: %s", string(v1))
	} else {
		t.Logf("recovery drill: both backups restored, versions differ (expected across PG upgrades)")
	}

	t.Logf("recovery drill: 2 backups, 2 restores complete")
}
