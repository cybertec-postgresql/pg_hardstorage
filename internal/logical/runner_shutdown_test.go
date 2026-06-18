package logical_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/logical"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// TestRun_WatcherTrackedInWaitGroup: regression guard for the
// v13 audit's "watcher goroutine not tracked in wg" finding.
//
// The bug: watchRegistry was started in its own goroutine but
// wasn't tracked in the supervisor wg. If ctx was cancelled at
// the exact moment a ticker fire was happening AND the rescan
// found a new stream to start, watchRegistry's startStream call
// would do `wg.Add(1)` AFTER `wg.Wait()` had already returned.
// That's the documented "Add called concurrently with Wait"
// misuse and triggers a Go runtime panic.
//
// We can't reliably reproduce the exact timing race in a unit
// test (the window is microseconds wide), but we CAN assert the
// stronger property the fix provides: Run() does not return
// until both the supervisors AND the watcher have exited. We
// detect "watcher still running" by observing that after Run
// returns, no further ticker-driven actions happen.
//
// Strategy: run with a tight rescan interval, register one
// stream, cancel the ctx, wait for Run to return, then verify
// no MORE rescan-driven events fire after the return.
func TestRun_WatcherTrackedInWaitGroup(t *testing.T) {
	mgr := newRunnerManager(t)

	var (
		mu     sync.Mutex
		events []*output.Event
	)
	r := &logical.Runner{
		Manager:        mgr,
		ConnectionFor:  func(*logical.Stream) string { return "" }, // orphan: supervisor exits fast
		RescanInterval: 10 * time.Millisecond,                      // fast ticker
		OnEvent: func(e *output.Event) {
			mu.Lock()
			events = append(events, e)
			mu.Unlock()
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	// Run should block until ctx expires; THEN return cleanly.
	// If the watcher were unsynchronised, Run could return early
	// while the watcher is still firing tickers — we'd observe
	// `stopped` followed by more events. With the fix, `stopped`
	// must be the LAST event.
	if err := r.Run(ctx); err != nil {
		t.Errorf("Run: %v", err)
	}

	// Snapshot the event log post-Run.
	mu.Lock()
	postRun := append([]*output.Event(nil), events...)
	mu.Unlock()

	// The last event must be "stopped". Anything after stopped
	// would mean the watcher (or a supervisor) emitted an event
	// after Run returned — the bug we're guarding against.
	if len(postRun) == 0 {
		t.Fatal("no events emitted; expected at least started + stopped")
	}
	last := postRun[len(postRun)-1]
	if last.Component != "logical.runner" || last.Op != "stopped" {
		t.Errorf("last event = %s/%s, want logical.runner/stopped (anything after stopped means a goroutine outlived Run)",
			last.Component, last.Op)
	}

	// Wait beyond the rescan interval and confirm no NEW events.
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	if len(events) != len(postRun) {
		extras := events[len(postRun):]
		var names []string
		for _, e := range extras {
			names = append(names, e.Component+"/"+e.Op)
		}
		t.Errorf("watcher still firing after Run returned; %d extra events: %v",
			len(extras), names)
	}
	mu.Unlock()
}

// TestRun_NoLeakedGoroutineOnEmptyRegistry: a tighter version
// of the same property. Empty registry, very fast rescan,
// shorter ctx — Run returns; nothing else should fire.
func TestRun_NoLeakedGoroutineOnEmptyRegistry(t *testing.T) {
	mgr := newRunnerManager(t)

	var (
		mu     sync.Mutex
		events []*output.Event
	)
	r := &logical.Runner{
		Manager:        mgr,
		ConnectionFor:  func(*logical.Stream) string { return "postgres://x" },
		RescanInterval: 5 * time.Millisecond,
		OnEvent: func(e *output.Event) {
			mu.Lock()
			events = append(events, e)
			mu.Unlock()
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := r.Run(ctx); err != nil {
		t.Errorf("Run: %v", err)
	}

	mu.Lock()
	beforeSleep := len(events)
	mu.Unlock()

	// Sleep through several rescan intervals; if the watcher
	// leaked, it'd be ticking and possibly emitting events.
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	if len(events) != beforeSleep {
		t.Errorf("event count grew after Run returned (%d → %d) — watcher goroutine leaked",
			beforeSleep, len(events))
	}
	mu.Unlock()
}
