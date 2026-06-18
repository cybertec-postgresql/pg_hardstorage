package backup

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestLease_RenewRefusesExpiredSelfLease pins race-condition audit #4: a
// holder whose own lease has lapsed past its TTL must treat it as lost on
// renewal rather than unconditionally reviving it. Renew's write is an
// unconditional overwrite, so reviving an expired lease would clobber a
// reclaimer that may be taking over in that window (split-brain). Here no
// reclaimer has acted yet, so the stored lease is still A's and the
// fencing-token check matches — the OLD code would happily renew it.
func TestLease_RenewRefusesExpiredSelfLease(t *testing.T) {
	sp := newLeaseSP(t)
	ctx := context.Background()
	clk := newClock()

	a, err := AcquireBackupLease(ctx, sp, "db1", LeaseOptions{Owner: "agent-A", TTL: 15 * time.Minute, now: clk.now})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// A renews on schedule while live — succeeds (the healthy path must
	// not regress).
	clk.advance(5 * time.Minute)
	if err := a.Renew(ctx); err != nil {
		t.Fatalf("renew within ttl must succeed; got %v", err)
	}

	// A stalls past the renewed TTL without renewing. Nobody has reclaimed
	// yet, so the lease on disk is still A's — but it has expired.
	clk.advance(16 * time.Minute)
	if err := a.Renew(ctx); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("renewing an expired self-lease must return ErrLeaseLost; got %v", err)
	}

	// Clobber-prevention proof: because A did NOT revive the lease, a fresh
	// acquirer can reclaim it. Under the old (revive) behaviour the lease
	// would be live again and this acquire would be refused.
	if _, err := AcquireBackupLease(ctx, sp, "db1", LeaseOptions{Owner: "agent-B", TTL: 15 * time.Minute, now: clk.now}); err != nil {
		t.Fatalf("a fresh acquirer must reclaim the un-revived expired lease; got %v", err)
	}
}

// TestLease_RenewExtendsWhileLive: the healthy renewal path is unchanged —
// repeated on-cadence renewals keep extending the expiry.
func TestLease_RenewExtendsWhileLive(t *testing.T) {
	sp := newLeaseSP(t)
	ctx := context.Background()
	clk := newClock()

	a, err := AcquireBackupLease(ctx, sp, "db1", LeaseOptions{Owner: "agent-A", TTL: 15 * time.Minute, now: clk.now})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// Five renewals at the TTL/3 cadence — each well within expiry.
	for i := 0; i < 5; i++ {
		clk.advance(5 * time.Minute)
		if err := a.Renew(ctx); err != nil {
			t.Fatalf("renewal %d at the TTL/3 cadence must succeed; got %v", i, err)
		}
	}
	// The lease's expiry has tracked forward, so a concurrent acquirer is
	// still blocked.
	if _, err := AcquireBackupLease(ctx, sp, "db1", LeaseOptions{Owner: "agent-B", TTL: 15 * time.Minute, now: clk.now}); !errors.Is(err, ErrBackupInProgress) {
		t.Fatalf("a continuously-renewed lease must keep blocking acquirers; got %v", err)
	}
}
