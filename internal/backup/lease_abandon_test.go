package backup

import (
	"context"
	"errors"
	"testing"
	"time"
)

// An acquisition that is cancelled (SIGINT, agent shutdown, a run ceiling)
// AFTER it has overwritten a stale/released lease with its own live body,
// but before its settle-verify finishes, used to return an error while
// leaving that body behind: a live lease, owned by nobody, that blocked
// every backup of the deployment for the whole TTL (15 min by default)
// and which no process would ever release. It must hand the lease back.
func TestLease_CancelledReclaimDoesNotLeaveLiveLease(t *testing.T) {
	sp := newLeaseSP(t)
	ctx := context.Background()

	// A first backup runs and releases cleanly: leaves a Released tombstone,
	// so every later acquire takes the reclaim path.
	first, err := AcquireBackupLease(ctx, sp, "db1", LeaseOptions{Owner: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Release(ctx); err != nil {
		t.Fatal(err)
	}

	cctx, cancel := context.WithCancel(ctx)
	leaseHookAfterReclaimPut = cancel // cancelled inside the settle window
	defer func() { leaseHookAfterReclaimPut = nil }()
	_, err = AcquireBackupLease(cctx, sp, "db1", LeaseOptions{Owner: "b", settle: 50 * time.Millisecond})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled acquire = %v, want context.Canceled", err)
	}
	leaseHookAfterReclaimPut = nil

	l := &Lease{sp: sp, deployment: "db1"}
	cur, err := l.read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !cur.Released && time.Now().Before(cur.ExpiresAt) {
		t.Fatalf("cancelled acquire left a LIVE lease behind (owner %q, expires %s): every backup of the deployment is now refused until it lapses", cur.Owner, cur.ExpiresAt)
	}
	// And the next acquire succeeds immediately.
	next, err := AcquireBackupLease(ctx, sp, "db1", LeaseOptions{Owner: "c", settle: 10 * time.Millisecond})
	if err != nil {
		t.Fatalf("acquire after a cancelled one: %v", err)
	}
	_ = next.Release(ctx)
}
