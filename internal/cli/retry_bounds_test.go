// retry_bounds_test.go — integration-style assertions that the
// wal-stream reconnect loop bounds itself correctly:
//
//   - On a permanent setup error (issue #79's class), the loop
//     exits within ONE attempt, not in unbounded retries.
//   - On transient errors, the loop backs off with the documented
//     exponential schedule and caps at maxReconnectBackoff.
//   - ctx cancellation during backoff exits within bound, not at
//     the next scheduled wake.
//
// Hand-rolled simulator: drives the exact retry primitives the
// real loop uses (isPermanentStreamSetupError, nextBackoff,
// sleepBackoff) via a scripted attempt source.  Refactoring
// runWalStream to accept a fake attempter would be invasive; the
// simulator covers the loop SHAPE without recompiling the agent.
//
// What this catches:
//   - A future change that classifies a permanent code as transient
//     (the predicate test in error_class_audit_test.go catches the
//     drift; this test catches the loop-behavior consequence).
//   - A future change to nextBackoff that lets it grow unbounded.
//   - A future change that races sleepBackoff against ctx so
//     cancellation is ignored.
package cli

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// runRetryLoopSimulator mirrors the control flow of runWalStream's
// retry block: per attempt, ask the attempter for an error, dispatch
// on isPermanentStreamSetupError, back off on transient.  Returns
// the attempt count when the loop exits and the last error seen.
//
// Differences from the real loop (kept minimal):
//   - No real I/O, no streamAttempt — just an attempter function
//     pulled from the test's script.
//   - No emit() of events — counted via the attempt counter.
func runRetryLoopSimulator(
	ctx context.Context,
	attempter func(attempt int) error,
	maxBackoff time.Duration,
) (attempts int, last error) {
	const initial = time.Millisecond // shrunk from 1s for test speed
	backoff := initial
	for {
		if ctx.Err() != nil {
			return attempts, ctx.Err()
		}
		attempts++
		err := attempter(attempts)
		last = err
		if err == nil {
			return attempts, nil
		}
		// The real loop short-circuits on a permanent setup error.
		if isPermanentStreamSetupError(err) {
			return attempts, err
		}
		if !sleepBackoff(ctx, backoff) {
			return attempts, ctx.Err()
		}
		backoff = nextBackoff(backoff, maxBackoff)
	}
}

// Permanent error: the loop must exit after exactly ONE attempt.
// Pre-issue-#79 the loop would spin forever; this test pins the
// bound at "1".
func TestRetryLoop_PermanentErrorExitsAfterOneAttempt(t *testing.T) {
	perm := output.NewError("wal.start_before_slot_restart_lsn",
		"computed start LSN sits behind slot's restart_lsn").
		Wrap(output.ErrUsage)

	attempts, last := runRetryLoopSimulator(
		context.Background(),
		func(int) error { return perm },
		time.Second,
	)
	if attempts != 1 {
		t.Errorf("permanent error should exit after 1 attempt; got %d (loop is back to the issue #79 spin)",
			attempts)
	}
	if !errors.Is(last, output.ErrUsage) {
		t.Errorf("expected ErrUsage chain; got %v", last)
	}
}

// All five currently-classified permanent codes must terminate the
// loop at attempt 1.  Catches a regression where the predicate
// keeps the name but flips the verdict.
func TestRetryLoop_AllPermanentCodesExitImmediately(t *testing.T) {
	perms := []string{
		"wal.start_before_slot_restart_lsn",
		"wal.slot_no_restart_lsn",
		"usage.bad_lsn",
		"usage.unaligned_lsn",
		"usage.bad_flag",
	}
	for _, code := range perms {
		t.Run(code, func(t *testing.T) {
			err := output.NewError(code, "synthetic for retry-bounds test").
				Wrap(output.ErrUsage)
			attempts, _ := runRetryLoopSimulator(
				context.Background(),
				func(int) error { return err },
				time.Second,
			)
			if attempts != 1 {
				t.Errorf("permanent code %q should exit at attempt 1; got %d", code, attempts)
			}
		})
	}
}

// Transient error: the loop must keep going.  ctx cancellation
// after ~10ms must STOP it within bound, not at the next scheduled
// backoff wake.
func TestRetryLoop_TransientThenCancel_BoundedExit(t *testing.T) {
	transient := output.NewError("connect.replication",
		"synthetic: simulated transient transport error")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()
	attempts, last := runRetryLoopSimulator(
		ctx,
		func(int) error { return transient },
		200*time.Millisecond,
	)
	elapsed := time.Since(start)

	// The loop should not have looped wildly: with the initial 1ms
	// backoff growing exponentially, we expect O(log) attempts.
	// 200ms ceiling > 20ms deadline, so backoff never caps, and
	// the loop should land cleanly within ~40ms (deadline + last
	// sleep) when ctx fires.
	if elapsed > 60*time.Millisecond {
		t.Errorf("ctx cancellation took %v to exit the loop; want <60ms (sleepBackoff is ignoring ctx)",
			elapsed)
	}
	if attempts < 2 {
		t.Errorf("only %d attempts in %v; transient retry loop didn't loop at all", attempts, elapsed)
	}
	if !errors.Is(last, context.DeadlineExceeded) {
		t.Errorf("expected DeadlineExceeded as final error; got %v", last)
	}
}

// nextBackoff caps strictly at the configured max no matter how
// many times it's called.  Catches an unbounded-growth regression.
func TestRetryLoop_BackoffCapsAtMax(t *testing.T) {
	cur := time.Millisecond
	max := 50 * time.Millisecond
	for i := 0; i < 100; i++ {
		cur = nextBackoff(cur, max)
		if cur > max {
			t.Fatalf("nextBackoff exceeded max after %d iterations: cur=%v max=%v", i, cur, max)
		}
	}
	if cur != max {
		t.Errorf("expected backoff to settle at max=%v; got %v", max, cur)
	}
}

// Success after N transient failures: loop terminates with nil and
// no further attempts.  Pins the happy-path eventual-recovery shape.
func TestRetryLoop_RecoversAfterTransientErrors(t *testing.T) {
	transient := output.NewError("connect.replication", "transient")
	const recoverAt = 4

	attempts, last := runRetryLoopSimulator(
		context.Background(),
		func(attempt int) error {
			if attempt < recoverAt {
				return transient
			}
			return nil
		},
		200*time.Millisecond,
	)
	if last != nil {
		t.Fatalf("loop returned error after recovery: %v", last)
	}
	if attempts != recoverAt {
		t.Errorf("expected exit on attempt %d (recovery); got %d", recoverAt, attempts)
	}
}
