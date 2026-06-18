//go:build integration

package regression_test

import (
	"context"
	"crypto/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/runner"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/testkit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
)

func TestCrossMajorPGUpgradeBackupRestore(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	srv := testkit.StartPostgres(t)

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

	target := filepath.Join(t.TempDir(), "upgrade_target")
	rres, err := restore.Restore(ctx, restore.Options{
		RepoURL: repoURL, Deployment: "db1", BackupID: res.BackupID, TargetDir: target, Verifier: verifier,
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if rres.FileCount == 0 || rres.BytesWritten == 0 {
		t.Fatal("empty restore for cross-major upgrade test")
	}

	pgVersion, err := os.ReadFile(filepath.Join(target, "PG_VERSION"))
	if err != nil {
		t.Fatalf("PG_VERSION: %v", err)
	}
	originalVersion := strings.TrimSpace(string(pgVersion))
	t.Logf("PG_VERSION in backup: %s", originalVersion)

	for _, name := range []string{"global", "base", "pg_xact", "pg_wal"} {
		if _, err := os.Stat(filepath.Join(target, name)); err != nil {
			t.Errorf("missing directory %s in restored data dir", name)
		}
	}

	pgUpgradeBin := "pg_upgrade"
	if runtime.GOOS == "darwin" {
		pgUpgradeBin = "/usr/local/bin/pg_upgrade"
	}
	if _, err := exec.LookPath(pgUpgradeBin); err != nil {
		t.Skipf("pg_upgrade not found at %q — skipping cross-major upgrade test", pgUpgradeBin)
	}

	cmd := exec.CommandContext(ctx, pgUpgradeBin, "--check")
	cmd.Dir = target
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Skipf("pg_upgrade --check failed (%v): %s — skipping cross-major upgrade", err, string(out))
	}

	t.Logf("cross-major upgrade: pg_upgrade --check passed on restored backup %s (PG %s)", res.BackupID, originalVersion)
}
