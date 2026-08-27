// Build-tagged integration test: the per-deployment backup lease under
// genuine concurrency against a real PG 17 testcontainer.
//
//go:build integration

package runner_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/runner"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/testkit"
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

	// The lease must be RELEASED once the winner completed — which
	// since the two-holder fix means a released TOMBSTONE, not an
	// absent key: Release overwrites rather than deletes, because a
	// deleted lease re-opens the claim-free create-if-absent path
	// mid-succession (see leaseBody.Released). What "released" must
	// mean to a caller is exactly two things, asserted below: the
	// stored body says so, and a follow-up backup can acquire.
	_, sp, err := repo.Open(ctx, repoURL)
	if err != nil {
		t.Fatalf("repo open: %v", err)
	}
	defer sp.Close()
	rc, gerr := sp.Get(ctx, "leases/db1/backup.json")
	if gerr != nil {
		t.Fatalf("read lease after backup: %v (the released tombstone must remain)", gerr)
	}
	var tomb struct {
		Released  bool      `json:"released"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	derr := json.NewDecoder(rc).Decode(&tomb)
	_ = rc.Close()
	if derr != nil {
		t.Fatalf("decode lease tombstone: %v", derr)
	}
	if !tomb.Released {
		t.Errorf("lease not marked released after the backup: %+v", tomb)
	}
	if time.Now().UTC().Before(tomb.ExpiresAt) {
		t.Errorf("released tombstone still reads unexpired (expires %s)", tomb.ExpiresAt)
	}

	// A sequential backup afterwards succeeds (the lease is free again).
	if _, err := runner.Take(ctx, runner.TakeOptions{
		PGConnString: srv.DSN, RepoURL: repoURL, Deployment: "db1",
		Signer: signer, Verifier: verifier, Fast: true,
	}); err != nil {
		t.Errorf("a backup after the lease is released should succeed: %v", err)
	}
}
