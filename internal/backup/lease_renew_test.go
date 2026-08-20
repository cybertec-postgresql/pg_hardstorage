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

// TestLease_RenewClockStepBackKeepsLease pins the crash-audit fix: a
// backward wall-clock step (NTP step, VM suspend/resume) between two
// renewals made now+ttl fall at-or-before the stored expiry and trip
// the old "renewal must extend expiry" invariant — a PANIC that killed
// the agent mid-backup. Renew now keeps the lease without writing
// (the would-be body is content-identical to the stored one, so no
// PUT, no shrunken window) and resumes extending once the clock
// passes cur.ExpiresAt - ttl.
func TestLease_RenewClockStepBackKeepsLease(t *testing.T) {
	sp := newLeaseSP(t)
	ctx := context.Background()
	clk := newClock()

	a, err := AcquireBackupLease(ctx, sp, "db1", LeaseOptions{Owner: "agent-A", TTL: 15 * time.Minute, now: clk.now})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// Healthy renewal on the TTL/3 cadence: expiry tracks forward.
	clk.advance(5 * time.Minute) // 12:05
	if err := a.Renew(ctx); err != nil {
		t.Fatalf("healthy renewal: %v", err)
	}
	healthyExpiry := a.body.ExpiresAt // 12:20

	// The NTP step: clock jumps 10 minutes BACKWARD, past the last
	// renewal. now+ttl (12:10) is now BEFORE the stored expiry (12:20).
	clk.advance(-10 * time.Minute) // 11:55

	renew := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Renew PANICKED on a backward clock step: %v", r)
			}
		}()
		err = a.Renew(ctx)
		return err
	}

	if err := renew(); err != nil {
		t.Fatalf("renewal across a clock step-back must not abort the healthy holder; got %v", err)
	}
	// No shrunken (or any) write: the stored lease still carries the
	// healthy 12:20 expiry.
	stored, err := a.read(ctx)
	if err != nil {
		t.Fatalf("read stored lease: %v", err)
	}
	if !stored.ExpiresAt.Equal(healthyExpiry) {
		t.Fatalf("stored expiry = %s, want unchanged %s — renewal must not shrink the window",
			stored.ExpiresAt.Format(time.RFC3339Nano), healthyExpiry.Format(time.RFC3339Nano))
	}
	// A second holder is still blocked — the lease is live in
	// wall-clock time.
	if _, err := AcquireBackupLease(ctx, sp, "db1", LeaseOptions{Owner: "agent-B", TTL: 15 * time.Minute, now: clk.now}); !errors.Is(err, ErrBackupInProgress) {
		t.Fatalf("lease must still block a second acquirer after the clock step; got %v", err)
	}
	// Forward again past cur.ExpiresAt - ttl: renewals extend normally.
	clk.advance(12 * time.Minute) // 12:07 → now+ttl = 12:22 > 12:20
	if err := renew(); err != nil {
		t.Fatalf("renewal after the clock recovered must extend; got %v", err)
	}
	if !a.body.ExpiresAt.After(healthyExpiry) {
		t.Fatalf("post-recovery renewal did not extend: %s <= %s",
			a.body.ExpiresAt.Format(time.RFC3339Nano), healthyExpiry.Format(time.RFC3339Nano))
	}
}
