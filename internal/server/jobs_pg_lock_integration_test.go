//go:build integration

package server

import (
	"context"
	"testing"
	"time"

	pgtestkit "github.com/cybertec-postgresql/pg_hardstorage/internal/pg/testkit"
)

// TestPGBackend_CappedClaimSerializesOnAdvisoryLock proves the mechanism
// behind the hard concurrency cap (race-condition audit #5): a
// concurrency-capped claim takes a transaction-scoped advisory lock so the
// running-count check and the claim are atomic w.r.t. other claims. While
// that lock is held elsewhere, a capped claim must BLOCK rather than race
// the count and overshoot. Under the old soft cap the claim took no lock
// and would not block — so this test fails against the pre-fix code.
//
// (An end-to-end "exactly cap wins under a concurrent burst" check lives
// in TestPGBackend_ConcurrencyCapIsHard; this one isolates the
// serialization primitive deterministically.)
func TestPGBackend_CappedClaimSerializesOnAdvisoryLock(t *testing.T) {
	pg := pgtestkit.StartPostgres(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b, err := OpenPGBackend(ctx, pg.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	if _, err := b.pool.Exec(ctx, `TRUNCATE phs.jobs`); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Enqueue(ctx, EnqueueOptions{Kind: JobBackup, Deployment: "db1"}); err != nil {
		t.Fatal(err)
	}

	// Hold the claim advisory lock in a side transaction.
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", claimAdvisoryLockKey); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, e := b.Claim(ctx, ClaimOptions{AgentID: "a", Deployments: []string{"db1"}, MaxConcurrent: 5})
		done <- e
	}()

	// While we hold the lock, the capped claim must block.
	select {
	case e := <-done:
		_ = tx.Rollback(ctx)
		t.Fatalf("capped Claim should block while the advisory lock is held; it returned early (err=%v)", e)
	case <-time.After(400 * time.Millisecond):
		// good — blocked on the advisory lock
	}

	// Release the lock; the claim must now proceed and succeed.
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-done:
		if e != nil {
			t.Fatalf("claim after the advisory lock was released: %v", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("claim did not proceed after the advisory lock was released")
	}
}
