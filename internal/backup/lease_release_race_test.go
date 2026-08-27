package backup

// lease_release_race_test.go — the delete-vs-succession race, pinned
// deterministically.
//
// Soak 23's integration phase reported "s3: 2 holders at once" from
// the randomized lease soak. The interleaving (reconstructed from the
// code, then forced here with the recheck hook):
//
//   1. holder H stalls past its TTL; its lease V1 is stale on disk
//   2. reclaimer A reads V1, passes its recheck        [A pauses here]
//   3. H wakes and Releases: read-check says V1 is still mine → DELETE
//   4. fresh acquirer C: create-if-absent SUCCEEDS (object gone) —
//      C holds, immediately, with no claim and no settle
//   5. A resumes: wins the break claim for V1 (nobody else claimed —
//      C never had to), overwrites C's live lease, settle-verifies
//      its own token back — A holds too
//
// Two backups of one deployment then run concurrently: the exact
// precondition for the dedup-vs-GC corruption family every commit
// gate exists to prevent. The fix removes step 3's delete: Release
// OVERWRITES the lease with a released tombstone that keeps the grant
// identity, so C's create-if-absent fails, C goes through the SAME
// grant-keyed break claim as A, and create-if-absent admits exactly
// one successor.
//
// This test replays the schedule with the hook and asserts at most
// one of A and C holds. Against the pre-fix code it fails with both
// holding.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestLease_ReleaseDuringReclaimCannotYieldTwoHolders(t *testing.T) {
	ctx := context.Background()
	sp := newLeaseSP(t)
	clk := newClock()
	const ttl = 15 * time.Minute
	opts := func(owner string) LeaseOptions {
		return LeaseOptions{Owner: owner, TTL: ttl, now: clk.now, settle: 50 * time.Millisecond}
	}

	// 1. H acquires and stalls past its TTL.
	h, err := AcquireBackupLease(ctx, sp, "db1", opts("H"))
	if err != nil {
		t.Fatalf("H acquire: %v", err)
	}
	clk.advance(ttl + time.Minute)

	// 2. A starts a stale reclaim and pauses after its recheck.
	pauseA := make(chan struct{})
	resumeA := make(chan struct{})
	var hookOnce sync.Once
	leaseHookAfterStaleRecheck = func() {
		fired := false
		hookOnce.Do(func() { fired = true })
		if fired {
			close(pauseA)
			<-resumeA
		}
	}
	defer func() { leaseHookAfterStaleRecheck = nil }()

	type result struct {
		lease *Lease
		err   error
	}
	aDone := make(chan result, 1)
	go func() {
		l, err := AcquireBackupLease(ctx, sp, "db1", opts("A"))
		aDone <- result{l, err}
	}()
	<-pauseA

	// 3. H releases — the delete (pre-fix) / tombstone (post-fix).
	if err := h.Release(ctx); err != nil {
		t.Fatalf("H release: %v", err)
	}

	// 4. C acquires fresh.
	cLease, cErr := AcquireBackupLease(ctx, sp, "db1", opts("C"))

	// 5. A resumes and finishes.
	close(resumeA)
	aRes := <-aDone

	holders := 0
	for _, r := range []result{{cLease, cErr}, aRes} {
		if r.err == nil && r.lease != nil {
			holders++
		} else if !errors.Is(r.err, ErrBackupInProgress) {
			t.Errorf("loser got %v, want ErrBackupInProgress", r.err)
		}
	}
	if holders != 1 {
		t.Fatalf("%d holders after the release-during-reclaim schedule, want exactly 1 — "+
			"two holders is the concurrent-backup precondition for the dedup-vs-GC "+
			"corruption family", holders)
	}
}
