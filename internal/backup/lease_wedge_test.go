package backup

// Covers the wedge detector against every succession state, including
// the one it exists for: claim consumed, winner dead, lease stale —
// forever "backup in progress" without intervention.

import (
	"context"
	"testing"
	"time"
)

func TestInspectLeaseSuccession(t *testing.T) {
	ctx := context.Background()

	t.Run("never leased", func(t *testing.T) {
		sp := newLeaseSP(t)
		w, err := InspectLeaseSuccession(ctx, sp, "db1", time.Now().UTC())
		if err != nil || w != nil {
			t.Fatalf("got %+v, %v; want nil, nil", w, err)
		}
	})

	t.Run("held live", func(t *testing.T) {
		sp := newLeaseSP(t)
		clk := newClock()
		if _, err := AcquireBackupLease(ctx, sp, "db1", LeaseOptions{Owner: "H", TTL: 15 * time.Minute, now: clk.now}); err != nil {
			t.Fatal(err)
		}
		w, err := InspectLeaseSuccession(ctx, sp, "db1", clk.now())
		if err != nil || w != nil {
			t.Fatalf("live lease flagged: %+v, %v", w, err)
		}
	})

	t.Run("released, no successor yet", func(t *testing.T) {
		sp := newLeaseSP(t)
		clk := newClock()
		l, err := AcquireBackupLease(ctx, sp, "db1", LeaseOptions{Owner: "H", TTL: 15 * time.Minute, now: clk.now})
		if err != nil {
			t.Fatal(err)
		}
		if err := l.Release(ctx); err != nil {
			t.Fatal(err)
		}
		w, err := InspectLeaseSuccession(ctx, sp, "db1", clk.now().Add(2*time.Hour))
		if err != nil || w != nil {
			t.Fatalf("released tombstone with no claim flagged: %+v, %v", w, err)
		}
	})

	t.Run("wedged: claim consumed, winner dead", func(t *testing.T) {
		sp := newLeaseSP(t)
		clk := newClock()
		const ttl = 15 * time.Minute
		l, err := AcquireBackupLease(ctx, sp, "db1", LeaseOptions{Owner: "H", TTL: ttl, now: clk.now})
		if err != nil {
			t.Fatal(err)
		}
		clk.advance(ttl + time.Minute) // H stalls past expiry

		// Reclaimer A wins the claim and dies before its overwrite:
		// simulate by claiming directly.
		var victim leaseBody
		victim, err = l.read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		a := &Lease{sp: sp, deployment: "db1", ttl: ttl, now: clk.now, settle: time.Millisecond}
		if err := a.claimBreak(ctx, victim, "A-who-died"); err != nil {
			t.Fatalf("claim: %v", err)
		}

		// Within the grace: not yet wedged (A might still be mid-put).
		if w, _ := InspectLeaseSuccession(ctx, sp, "db1", clk.now().Add(time.Minute)); w != nil {
			t.Fatalf("flagged wedged %v after only a minute — a live reclaimer would be deleted out from under", w)
		}
		// Past the grace: wedged, with both keys named.
		w, err := InspectLeaseSuccession(ctx, sp, "db1", clk.now().Add(2*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if w == nil {
			t.Fatal("dead-winner succession not detected — every future acquire reports " +
				"backup-in-progress forever and nothing says why")
		}
		if w.ClaimKey == "" || w.LeaseKey == "" || w.Breaker != "A-who-died" {
			t.Fatalf("wedge report incomplete: %+v", w)
		}
		// The remedy must work: delete the claim, and acquisition heals.
		if err := sp.Delete(ctx, w.ClaimKey); err != nil {
			t.Fatal(err)
		}
		if _, err := AcquireBackupLease(ctx, sp, "db1", LeaseOptions{Owner: "C", TTL: ttl, now: clk.now, settle: time.Millisecond}); err != nil {
			t.Fatalf("acquire after clearing the wedge: %v", err)
		}
	})
}
