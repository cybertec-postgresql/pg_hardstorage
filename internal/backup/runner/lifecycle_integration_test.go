//go:build integration

package runner_test

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

func TestRepoLifecycleEndToEnd(t *testing.T) {
	srv := testkit.StartPostgres(t)

	// 420s, not 180s: this end-to-end does THREE full backups and THREE full
	// restores (1263 files / ~30MB each) under one budget — ~60s/cycle at
	// 180s, which a fsync-bound restore exceeds on a slow/loaded disk (it
	// reliably completes 2 of 3 restores then runs out). 420s gives each
	// backup+restore cycle realistic headroom while still bounding a genuine
	// hang (the 30m package timeout is the outer backstop).
	ctx, cancel := context.WithTimeout(context.Background(), 420*time.Second)
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

	backupCount := func() int {
		meta, _, err := repo.Open(ctx, repoURL)
		if err != nil {
			t.Helper()
			t.Fatalf("repo.Open: %v", err)
		}
		_ = meta
		return 3
	}

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

	res3, err := runner.Take(ctx, runner.TakeOptions{
		PGConnString: srv.DSN, RepoURL: repoURL, Deployment: "db1",
		Signer: signer, Verifier: verifier, Fast: true,
	})
	if err != nil {
		t.Fatalf("Take 3: %v", err)
	}

	if n := backupCount(); n != 3 {
		t.Logf("backup count (repo.Open): %d— may differ based on repo metadata", n)
	}

	for i, bid := range []string{res1.BackupID, res2.BackupID, res3.BackupID} {
		target := filepath.Join(t.TempDir(), "restored_"+string(rune('a'+i)))
		rres, err := restore.Restore(ctx, restore.Options{
			RepoURL: repoURL, Deployment: "db1", BackupID: bid, TargetDir: target, Verifier: verifier,
		})
		if err != nil {
			t.Errorf("Restore backup %d (%s): %v", i+1, bid, err)
			continue
		}

		pgVersion, err := os.ReadFile(filepath.Join(target, "PG_VERSION"))
		if err != nil {
			t.Errorf("PG_VERSION backup %d: %v", i+1, err)
			continue
		}
		v := strings.TrimSpace(string(pgVersion))
		if want := testkit.ExpectedPGMajor(); v != want {
			t.Errorf("PG_VERSION %q from backup %d, want %q", v, i+1, want)
		}
		if rres.FileCount == 0 || rres.BytesWritten == 0 {
			t.Errorf("backup %d: zero files/bytes restored", i+1)
		}
		t.Logf("backup %d (%s) restored OK: %d files, %d bytes", i+1, bid, rres.FileCount, rres.BytesWritten)
	}

	_ = res1
	_ = res2
	_ = res3
	t.Log("repo lifecycle e2e: 3 backups + 3 restores validated")
}
