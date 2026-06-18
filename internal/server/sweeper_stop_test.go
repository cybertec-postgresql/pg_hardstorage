package server_test

import (
	"context"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/server"
)

// TestRunSweeper_StopWaitsForGoroutine: cancel the context the
// sweeper is using, then call Stop() and assert it returns
// promptly (within 1s — generous; the sweeper exits its loop
// on the next select after ctx.Done). Without WaitGroup
// tracking the goroutine outlives the test and `-race` flags
// it.
func TestRunSweeper_StopWaitsForGoroutine(t *testing.T) {
	r := server.NewJobRegistry()
	ctx, cancel := context.WithCancel(context.Background())
	r.RunSweeper(ctx, time.Hour, nil) // long interval — ctx is the only exit

	cancel()
	done := make(chan struct{})
	go func() {
		r.Stop()
		close(done)
	}()
	select {
	case <-done:
		// good
	case <-time.After(time.Second):
		t.Fatal("Stop() did not return within 1s after ctx cancel")
	}
}

// TestRunSweeper_StopBeforeRunSweeper_NoOp: Stop on a
// registry with no sweeper attached returns immediately.
// Wait on a zero-counter WaitGroup is a no-op.
func TestRunSweeper_StopBeforeRunSweeper_NoOp(t *testing.T) {
	r := server.NewJobRegistry()
	done := make(chan struct{})
	go func() {
		r.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Stop() should be a no-op when no sweeper was started")
	}
}

// TestRunSweeper_StopWithoutCtxCancel pins deadlock audit #1: Stop()
// returns promptly even when the caller NEVER cancels the ctx it passed
// to RunSweeper, because Stop cancels the (derived) sweeper context
// itself. Against the old Stop() (a bare sweeperWG.Wait()) this hangs
// forever, caught here by the 2s timeout.
func TestRunSweeper_StopWithoutCtxCancel(t *testing.T) {
	r := server.NewJobRegistry()
	// context.Background() is never cancelled; a long interval means the
	// ticker won't fire — the goroutine's only exit is Stop's cancel.
	r.RunSweeper(context.Background(), time.Hour, nil)

	done := make(chan struct{})
	go func() {
		r.Stop()
		close(done)
	}()
	select {
	case <-done:
		// good — Stop self-cancelled and returned
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() hung with an un-cancelled sweeper ctx — it must self-cancel")
	}
}

// TestRunSweeper_StopIsIdempotent: a Stop after the caller also cancelled
// ctx, and a second Stop, are harmless no-ops.
func TestRunSweeper_StopIsIdempotent(t *testing.T) {
	r := server.NewJobRegistry()
	ctx, cancel := context.WithCancel(context.Background())
	r.RunSweeper(ctx, time.Hour, nil)

	cancel() // caller cancels too — Stop must still be fine
	r.Stop()
	r.Stop() // second call: no panic, no hang
}

// TestRunSweeper_MultipleStartsAllTracked: calling RunSweeper
// twice (e.g. tests that re-arm) registers two goroutines;
// Stop drains both. Without per-call Add(1) the second
// goroutine would leak.
func TestRunSweeper_MultipleStartsAllTracked(t *testing.T) {
	r := server.NewJobRegistry()
	ctx, cancel := context.WithCancel(context.Background())
	r.RunSweeper(ctx, time.Hour, nil)
	r.RunSweeper(ctx, time.Hour, nil)

	cancel()
	done := make(chan struct{})
	go func() {
		r.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Stop() should drain BOTH sweeper goroutines")
	}
}
