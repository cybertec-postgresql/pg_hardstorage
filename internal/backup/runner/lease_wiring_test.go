package runner

import (
	"context"
	"crypto/rand"
	"errors"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// TestTake_RefusedWhenLeaseHeld proves the runner acquires the backup
// lease — and refuses — before it touches PostgreSQL. We pre-seed a
// live lease (as if another agent were mid-backup) and call Take with a
// bogus PG connection string: because the lease is acquired right after
// the repo opens and before any PG work, Take fails fast with
// backup.ErrBackupInProgress rather than trying (and failing) to
// connect to PG.
func TestTake_RefusedWhenLeaseHeld(t *testing.T) {
	ctx := context.Background()
	repoURL := "file://" + t.TempDir()
	if _, err := repo.Init(ctx, repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatalf("repo init: %v", err)
	}

	// Another holder is mid-backup of db1.
	_, sp, err := repo.Open(ctx, repoURL)
	if err != nil {
		t.Fatalf("repo open: %v", err)
	}
	if _, err := backup.AcquireBackupLease(ctx, sp, "db1", backup.LeaseOptions{Owner: "other-agent"}); err != nil {
		t.Fatalf("seed lease: %v", err)
	}
	_ = sp.Close()

	priv, _, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := backup.LoadSigner(priv)

	_, err = Take(ctx, TakeOptions{
		PGConnString: "postgres://unused-because-we-fail-first",
		RepoURL:      repoURL,
		Deployment:   "db1",
		Signer:       signer,
	})
	if err == nil {
		t.Fatal("Take must refuse while a live lease is held for the deployment")
	}
	if !errors.Is(err, backup.ErrBackupInProgress) {
		t.Fatalf("Take error = %v, want ErrBackupInProgress", err)
	}
}

// TestTake_SkipLease_DoesNotRefuse confirms the SkipLease escape hatch
// bypasses the lease: with a live lease held, Take proceeds past the
// lease step (and then fails later for an unrelated reason — the bogus
// PG connection — NOT with ErrBackupInProgress).
func TestTake_SkipLease_DoesNotRefuse(t *testing.T) {
	ctx := context.Background()
	repoURL := "file://" + t.TempDir()
	if _, err := repo.Init(ctx, repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatalf("repo init: %v", err)
	}
	_, sp, err := repo.Open(ctx, repoURL)
	if err != nil {
		t.Fatalf("repo open: %v", err)
	}
	if _, err := backup.AcquireBackupLease(ctx, sp, "db1", backup.LeaseOptions{Owner: "other-agent"}); err != nil {
		t.Fatalf("seed lease: %v", err)
	}
	_ = sp.Close()

	priv, _, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := backup.LoadSigner(priv)

	_, err = Take(ctx, TakeOptions{
		PGConnString: "postgres://unused-bogus-host-nonexistent:1/db",
		RepoURL:      repoURL,
		Deployment:   "db1",
		Signer:       signer,
		SkipLease:    true,
	})
	if err == nil {
		t.Fatal("Take with a bogus PG conn should still fail somewhere")
	}
	if errors.Is(err, backup.ErrBackupInProgress) {
		t.Fatalf("SkipLease should bypass the lease, but Take returned ErrBackupInProgress")
	}
}
