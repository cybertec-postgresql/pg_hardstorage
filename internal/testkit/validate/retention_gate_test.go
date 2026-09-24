package validate

import (
	"context"
	"testing"
	"time"
)

func TestRetentionGate_QuiesceTimesOutWhileBusyAndReleasesHeldWork(t *testing.T) {
	g := newRetentionGate()
	ctx := context.Background()
	if !g.enter(ctx) {
		t.Fatal("enter on an idle gate must succeed")
	}
	start := time.Now()
	if g.quiesce(ctx, 30*time.Millisecond) {
		t.Fatal("quiesce must not succeed while an operation is in flight")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("quiesce overran its timeout: %s", time.Since(start))
	}
	// A timed-out window must not keep holding new work.
	done := make(chan bool, 1)
	go func() { done <- g.enter(ctx) }()
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("enter after a timed-out window failed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a timed-out window kept holding new work")
	}
}

func TestRetentionGate_QuiesceWaitsForDrainAndHoldsNewWork(t *testing.T) {
	g := newRetentionGate()
	ctx := context.Background()
	g.enter(ctx)
	go func() { time.Sleep(20 * time.Millisecond); g.leave() }()
	if !g.quiesce(ctx, 2*time.Second) {
		t.Fatal("quiesce must succeed once in-flight work drains")
	}
	entered := make(chan struct{})
	go func() { g.enter(ctx); close(entered) }()
	select {
	case <-entered:
		t.Fatal("new work entered while the window was open")
	case <-time.After(50 * time.Millisecond):
	}
	g.resume()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("resume did not release held work")
	}
}

func TestRetentionGate_EnterGivesUpWhenTheRunEnds(t *testing.T) {
	g := newRetentionGate()
	if !g.quiesce(context.Background(), time.Second) {
		t.Fatal("quiesce on an idle gate must succeed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() { done <- g.enter(ctx) }()
	cancel()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("enter must report false when its context ends")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("enter blocked past its context")
	}
}
