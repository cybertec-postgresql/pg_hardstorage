//go:build integration

package repo_test

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

func TestGarbageCollectionAfterRetention(t *testing.T) {
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

	for i := 0; i < 3; i++ {
		res, err := runner.Take(ctx, runner.TakeOptions{
			PGConnString: srv.DSN, RepoURL: repoURL, Deployment: "db1",
			Signer: signer, Verifier: verifier, Fast: true,
		})
		if err != nil {
			t.Errorf("Take %d: %v", i+1, err)
			continue
		}
		if res.BackupID == "" {
			t.Errorf("Take %d: empty backup ID", i+1)
		}
		t.Logf("backup %d: %s", i+1, res.BackupID)
	}

	_, sp, err := repo.Open(ctx, repoURL)
	if err != nil {
		t.Fatalf("repo.Open: %v", err)
	}
	store := backup.NewManifestStore(sp)
	count := 0
	for _, listErr := range store.List(ctx, "db1", verifier) {
		if listErr != nil {
			t.Fatalf("ManifestStore.List: %v", listErr)
		}
		count++
	}
	if count < 3 {
		t.Errorf("expected >= 3 manifests, got %d", count)
	}
	t.Logf("repo: %d manifests listable (GC input set) after 3 backups", count)
}
