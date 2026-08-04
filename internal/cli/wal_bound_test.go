// wal_bound_test.go — the bound that keeps a stalled `wal stream` from
// taking the package down with it.
//
// This is a test of test infrastructure, which normally is not worth
// writing. It is here because of what happened without it: an
// unbounded `wal stream --once` hung for 6m26s, go test killed the
// whole internal/cli package at its 10m limit, and the report named one
// arbitrary test while hiding the several hundred it took with it. The
// bound is what converts that into one named failure — so if the bound
// silently stopped working, the thing that goes missing is the
// diagnosis, not a test result. Nothing else would notice.
//
// awaitBounded is the decision, separated from the t.Fatalf so it can
// be driven directly.
//
//go:build integration

package cli_test

import (
	"testing"
	"time"
)

// TestAwaitBounded_ReturnsExitCodeWhenCommandFinishes is the ordinary
// path: every passing wal-stream test goes through it.
func TestAwaitBounded_ReturnsExitCodeWhenCommandFinishes(t *testing.T) {
	for _, want := range []int{0, 1, 8} {
		done := make(chan int, 1)
		done <- want
		got, returned := awaitBounded(done, time.Minute)
		if !returned {
			t.Fatalf("exit %d: reported a timeout for a command that had already returned", want)
		}
		if got != want {
			t.Errorf("exit code = %d, want %d — the assertions in every wal-stream test "+
				"are made against this value", got, want)
		}
	}
}

// TestAwaitBounded_ReportsTimeoutWhenCommandStalls is the path that
// matters, and the one that had never run: a command that never
// returns must be reported as a timeout rather than blocking forever.
func TestAwaitBounded_ReportsTimeoutWhenCommandStalls(t *testing.T) {
	never := make(chan int) // nothing ever sends

	start := time.Now()
	_, returned := awaitBounded(never, 150*time.Millisecond)
	elapsed := time.Since(start)

	if returned {
		t.Fatal("reported that a stalled command returned; the bound would never fire and " +
			"a hung `wal stream` would run until the package timeout, failing every test " +
			"after it")
	}
	if elapsed < 150*time.Millisecond {
		t.Errorf("gave up after %s, before the %s budget — a bound that fires early turns "+
			"a slow runner into a red build", elapsed, 150*time.Millisecond)
	}
	if elapsed > 5*time.Second {
		t.Errorf("took %s to report a %s timeout", elapsed, 150*time.Millisecond)
	}
}

// TestAwaitBounded_PrefersACompletedCommand covers the race at the
// boundary: a command that finishes just as the budget expires must be
// reported as finished.
//
// Reporting a timeout for a command that DID return would fail a test
// whose subject worked perfectly — the most confusing outcome
// available here, because the failure message would describe a stall
// that did not happen.
func TestAwaitBounded_PrefersACompletedCommand(t *testing.T) {
	// Run it repeatedly: the interesting interleaving is timing
	// dependent, and once is not evidence.
	for i := 0; i < 50; i++ {
		done := make(chan int, 1)
		done <- 0
		if _, returned := awaitBounded(done, time.Nanosecond); !returned {
			t.Fatalf("iteration %d: a command whose result was already available was "+
				"reported as stalled", i)
		}
	}
}

// TestAwaitBounded_DoesNotWaitOutTheBudget pins that the common case
// is fast. If it always waited the full budget, the wal-stream suite
// would take 6 × 3 minutes even when everything passes, and the
// obvious fix — shrinking the budget — would reintroduce the
// false-timeout failures the budgets were widened to stop.
func TestAwaitBounded_DoesNotWaitOutTheBudget(t *testing.T) {
	done := make(chan int, 1)
	go func() {
		time.Sleep(20 * time.Millisecond)
		done <- 0
	}()

	start := time.Now()
	if _, returned := awaitBounded(done, 30*time.Second); !returned {
		t.Fatal("timed out on a command that returned in 20ms")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("returned after %s for a command that finished in 20ms; the helper is "+
			"waiting out its budget instead of the command", elapsed)
	}
}
