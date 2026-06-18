//go:build integration

package standby_test

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

func TestStandbyRestoreFromBackupPipeline(t *testing.T) {
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

	for i := 0; i < 2; i++ {
		target := filepath.Join(t.TempDir(), "standby_"+string(rune('a'+i)))
		rres, err := restore.Restore(ctx, restore.Options{
			RepoURL: repoURL, Deployment: "db1", BackupID: res.BackupID,
			TargetDir: target, Verifier: verifier,
		})
		if err != nil {
			t.Errorf("Restore %d: %v", i+1, err)
			continue
		}

		for _, name := range []string{"PG_VERSION", "global/pg_control"} {
			if _, err := os.Stat(filepath.Join(target, name)); err != nil {
				t.Errorf("standby restore %d: missing %s", i+1, name)
			}
		}
		t.Logf("standby restore %d: %d files, %d bytes", i+1, rres.FileCount, rres.BytesWritten)
	}

	t.Logf("standby: 2 restores from backup pipeline OK")
}
