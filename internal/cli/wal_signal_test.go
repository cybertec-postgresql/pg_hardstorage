package cli

import (
	"context"
	"syscall"
	"testing"
	"time"
)

// Regression (concurrency audit): the first SIGINT must trigger the
// GRACEFUL stop path — gracefulStarted flips, the graceful goroutine
// runs to completion (closing gracefulDone), and only then does the
// stream ctx cancel. Previously the root signal ctx cancelled the
// stream first and the handler's select could take the ctx.Done arm,
// skipping pg_switch_wal entirely.
func TestInstallSignalCancel_FirstSignalRunsGracefulPath(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	shutdown := &walShutdown{gracefulDone: make(chan struct{})}
	// Empty DSN → gracefulStopAndCancel returns immediately after
	// (deferred) cancel, keeping the test hermetic (no PG needed).
	installSignalCancel(ctx, cancel, shutdown)

	// Deliver a real SIGINT to ourselves; signal.Notify routes it to
	// the handler's channel (no default-death while registered).
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGINT); err != nil {
		t.Fatalf("self-SIGINT: %v", err)
	}

	select {
	case <-shutdown.gracefulDone:
	case <-time.After(5 * time.Second):
		t.Fatal("graceful goroutine never completed after first SIGINT")
	}
	if !shutdown.gracefulStarted.Load() {
		t.Error("gracefulStarted not set — the signal took the non-graceful path")
	}
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Error("stream ctx not cancelled after graceful stop completed")
	}
}

// When the stream ends on its own (ctx cancelled without any signal),
// the handler must close gracefulDone so the result path's wait can
// never hang, and gracefulStarted must stay false.
func TestInstallSignalCancel_NoSignalExitClosesDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	shutdown := &walShutdown{gracefulDone: make(chan struct{})}
	installSignalCancel(ctx, cancel, shutdown)

	cancel() // stream ended by itself
	select {
	case <-shutdown.gracefulDone:
	case <-time.After(3 * time.Second):
		t.Fatal("gracefulDone not closed on the no-signal exit path (result wait would hang)")
	}
	if shutdown.gracefulStarted.Load() {
		t.Error("gracefulStarted set without a signal")
	}
}
