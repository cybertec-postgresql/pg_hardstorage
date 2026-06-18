// Build-tagged integration test: the per-deployment backup lease under
// genuine concurrency against a real PG 17 testcontainer.
//
//go:build integration

package runner_test

import (
	"context"
	"crypto/rand"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/runner"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/testkit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// TestIntegration_BackupLease_RefusesConcurrentSameDeployment launches
// several real backups of the SAME deployment at once. Exactly one
// acquires the lease and runs; the rest are refused fast with
// ErrBackupInProgress (before they ever touch PostgreSQL). After the
// winner finishes, the lease is released — no stale marker remains.
func TestIntegration_BackupLease_RefusesConcurrentSameDeployment(t *testing.T) {
	srv := testkit.StartPostgres(t)

	repoURL := "file://" + t.TempDir()
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatalf("repo init: %v", err)
	}
	priv, pub, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	const N = 4
	var (
		wg      sync.WaitGroup
		barrier = make(chan struct{})
		results = make([]error, N)
	)
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			<-barrier // release all goroutines into Take at once
			_, err := runner.Take(ctx, runner.TakeOptions{
				PGConnString: srv.DSN,
				RepoURL:      repoURL,
				Deployment:   "db1",
				Signer:       signer,
				Verifier:     verifier,
				Fast:         true,
				LeaseOwner:   "racer",
			})
			results[i] = err
		}(i)
	}
	close(barrier)
	wg.Wait()

	var wins, refused atomic.Int32
	for _, err := range results {
		switch {
		case err == nil:
			wins.Add(1)
		case errors.Is(err, backup.ErrBackupInProgress):
			refused.Add(1)
		default:
			t.Errorf("unexpected backup error: %v", err)
		}
	}
	if wins.Load() != 1 {
		t.Errorf("exactly one concurrent backup of the same deployment should win; got %d", wins.Load())
	}
	if refused.Load() != N-1 {
		t.Errorf("the rest should be refused with ErrBackupInProgress; got %d of %d", refused.Load(), N-1)
	}

	// The lease must be released once the winner completed.
	_, sp, err := repo.Open(ctx, repoURL)
	if err != nil {
		t.Fatalf("repo open: %v", err)
	}
	defer sp.Close()
	if _, err := sp.Stat(ctx, "leases/db1/backup.json"); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("backup lease should be released after the backup; stat err = %v (want ErrNotFound)", err)
	}

	// A sequential backup afterwards succeeds (the lease is free again).
	if _, err := runner.Take(ctx, runner.TakeOptions{
		PGConnString: srv.DSN, RepoURL: repoURL, Deployment: "db1",
		Signer: signer, Verifier: verifier, Fast: true,
	}); err != nil {
		t.Errorf("a backup after the lease is released should succeed: %v", err)
	}
}
