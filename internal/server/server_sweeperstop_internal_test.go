package server

import (
	"context"
	"testing"
	"time"
)

// sweeperCount reports how many sweeper cancel funcs the registry is
// currently tracking. Stop() clears them to nil, so a zero count after
// Run returns means the sweeper was stopped.
func (r *JobRegistry) sweeperCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sweeperCancels)
}

// TestRun_StopsSweeperWhenServeReturns is the regression for bug #54:
// Run started the job sweeper bound only to ctx and never called
// jobs.Stop(), so when Serve returned on its own (an external
// srv.Close, or a listener error) WITHOUT a ctx cancel, the sweeper
// kept ticking against a possibly-Closed backend. After the fix Run
// defers jobs.Stop(), so the sweeper is drained on every exit path.
//
// Internal (package server) test: it reaches s.srv to force the Serve
// return without cancelling ctx, then inspects the registry's tracked
// sweeper goroutines.
func TestRun_StopsSweeperWhenServeReturns(t *testing.T) {
	s, err := New(Config{Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}

	// A live context we deliberately do NOT cancel: shutdown is driven
	// by Serve returning, not by ctx. This is the exact path where the
	// old code leaked the sweeper.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	// Let Run bind, start the sweeper, and reach Serve.
	time.Sleep(150 * time.Millisecond)
	if got := s.jobs.sweeperCount(); got == 0 {
		t.Fatal("sweeper was never started")
	}
	_ = s.srv.Close()

	select {
	case <-done:
		// Run returned.
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after Serve closed")
	}

	if got := s.jobs.sweeperCount(); got != 0 {
		t.Fatalf("sweeper still tracked after Run returned (count=%d); jobs.Stop() was not called", got)
	}
}
