package server

import (
	"context"
	"testing"
	"time"
)

// TestRun_ServeReturnsWithoutCancelDoesNotHang pins deadlock audit #2:
// when Serve returns on its own (here via an external srv.Close) WITHOUT
// the run context being cancelled, Run must still return — not block
// forever on <-shutdownDone waiting for a shutdown goroutine that is
// parked on <-ctx.Done(). The fix wakes that goroutine via serveDone.
//
// This is an internal (package server) test because it reaches s.srv to
// force the Serve return without touching the run context.
func TestRun_ServeReturnsWithoutCancelDoesNotHang(t *testing.T) {
	s, err := New(Config{Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}

	// A live context that we deliberately do NOT cancel during the test —
	// shutdown is triggered by Serve returning, not by ctx.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	// Let Run bind and reach Serve, then make Serve return on its own.
	time.Sleep(150 * time.Millisecond)
	_ = s.srv.Close()

	select {
	case <-done:
		// good — Run returned even though ctx was never cancelled
	case <-time.After(3 * time.Second):
		t.Fatal("Run hung after Serve returned without a ctx cancel — shutdown goroutine never released")
	}
}
