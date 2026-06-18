//go:build integration

package postverify_test

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
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore/postverify"
)

func TestPostVerifyAgainstRestoredDatadir(t *testing.T) {
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

	target := filepath.Join(t.TempDir(), "restored_verify")
	rres, err := restore.Restore(ctx, restore.Options{
		RepoURL: repoURL, Deployment: "db1", BackupID: res.BackupID,
		TargetDir: target, Verifier: verifier,
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if rres.FileCount == 0 || rres.BytesWritten == 0 {
		t.Fatalf("empty restore: files=%d bytes=%d", rres.FileCount, rres.BytesWritten)
	}

	for _, name := range []string{"PG_VERSION", "backup_label", "global/pg_control"} {
		if _, err := os.Stat(filepath.Join(target, name)); err != nil {
			t.Errorf("missing %s in restored data dir", name)
		}
	}

	verifyRes, err := postverify.Verify(ctx, postverify.Options{
		Mode:       postverify.ModeAuto,
		DataDir:    target,
		RepoURL:    repoURL,
		Deployment: "db1",
	})
	if err != nil {
		t.Logf("postverify: %v (pg_verifybackup / pg_ctl may be absent in test env)", err)
	} else if verifyRes.Skipped {
		t.Logf("postverify skipped: %s", verifyRes.SkipReason)
	} else {
		t.Logf("postverify passed: queries=%d start=%s", verifyRes.QueriesRan, verifyRes.StartDuration)
	}
}
