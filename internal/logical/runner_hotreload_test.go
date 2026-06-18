package logical_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/logical"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// TestRun_HotReloadPicksUpNewStream is the operator-visible
// contract: an `pg_hardstorage logical add` while the agent is up
// causes the new stream to start without an agent restart.
//
// We drive it by:
//  1. starting Run with one registered stream (with no connection,
//     so its supervisor exits immediately and the goroutine
//     drains)
//  2. adding a second stream after Run is already going
//  3. asserting a stream.added event lands within a few ticks
func TestRun_HotReloadPicksUpNewStream(t *testing.T) {
	mgr := newRunnerManager(t)
	if _, err := mgr.Add(logical.AddOptions{
		Name:        "first",
		Deployment:  "no-conn",
		Slot:        "s1",
		Plugin:      "pgoutput",
		Publication: "pub",
		SinkKind:    "chunked",
		RepoURL:     "file:///tmp/whatever",
	}); err != nil {
		t.Fatal(err)
	}

	var (
		mu     sync.Mutex
		events []*output.Event
	)
	r := &logical.Runner{
		Manager:        mgr,
		ConnectionFor:  func(*logical.Stream) string { return "" }, // skip the supervisor
		RescanInterval: 25 * time.Millisecond,
		OnEvent: func(e *output.Event) {
			mu.Lock()
			events = append(events, e)
			mu.Unlock()
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan struct{})
	go func() {
		_ = r.Run(ctx)
		close(runDone)
	}()

	// Wait for the started event, then add a second stream.
	if !waitForEvent(t, &mu, &events, "logical.runner", "started", 1*time.Second) {
		t.Fatal("never saw started event")
	}

	// Give the watcher one tick to settle, then add.
	time.Sleep(60 * time.Millisecond)
	if _, err := mgr.Add(logical.AddOptions{
		Name:        "second",
		Deployment:  "no-conn",
		Slot:        "s2",
		Plugin:      "pgoutput",
		Publication: "pub2",
		SinkKind:    "chunked",
		RepoURL:     "file:///tmp/whatever",
	}); err != nil {
		t.Fatal(err)
	}

	if !waitForEvent(t, &mu, &events, "logical.runner", "stream.added", 2*time.Second) {
		t.Fatal("watcher didn't detect the newly-added stream within 2s")
	}

	cancel()
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Run didn't exit within 2s of ctx cancel")
	}
}

// TestRun_HotReloadCancelsRemovedStream: removing a stream from
// the registry cancels its supervisor without affecting other
// streams.
func TestRun_HotReloadCancelsRemovedStream(t *testing.T) {
	mgr := newRunnerManager(t)
	for _, n := range []string{"alpha", "beta"} {
		if _, err := mgr.Add(logical.AddOptions{
			Name:        n,
			Deployment:  "no-conn",
			Slot:        "slot-" + n,
			Plugin:      "pgoutput",
			Publication: "pub",
			SinkKind:    "chunked",
			RepoURL:     "file:///tmp/whatever",
		}); err != nil {
			t.Fatal(err)
		}
	}

	var (
		mu     sync.Mutex
		events []*output.Event
	)
	r := &logical.Runner{
		Manager:        mgr,
		ConnectionFor:  func(*logical.Stream) string { return "" },
		RescanInterval: 25 * time.Millisecond,
		OnEvent: func(e *output.Event) {
			mu.Lock()
			events = append(events, e)
			mu.Unlock()
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan struct{})
	go func() {
		_ = r.Run(ctx)
		close(runDone)
	}()

	if !waitForEvent(t, &mu, &events, "logical.runner", "started", 1*time.Second) {
		t.Fatal("never saw started event")
	}
	time.Sleep(60 * time.Millisecond)

	if err := mgr.Remove("alpha"); err != nil {
		t.Fatal(err)
	}

	if !waitForEvent(t, &mu, &events, "logical.runner", "stream.removed", 2*time.Second) {
		t.Fatal("watcher didn't detect the removed stream within 2s")
	}

	cancel()
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Run didn't exit within 2s of ctx cancel")
	}
}

// TestRun_HotReloadDisabledByNegativeInterval — passing a negative
// RescanInterval skips the watcher loop. Useful for tests + for
// operators who want the legacy "list at startup, never rescan"
// behaviour back.
func TestRun_HotReloadDisabledByNegativeInterval(t *testing.T) {
	mgr := newRunnerManager(t)
	if _, err := mgr.Add(logical.AddOptions{
		Name:        "only",
		Deployment:  "no-conn",
		Slot:        "s1",
		Plugin:      "pgoutput",
		Publication: "pub",
		SinkKind:    "chunked",
		RepoURL:     "file:///tmp/whatever",
	}); err != nil {
		t.Fatal(err)
	}

	var (
		mu     sync.Mutex
		events []*output.Event
	)
	r := &logical.Runner{
		Manager:        mgr,
		ConnectionFor:  func(*logical.Stream) string { return "" },
		RescanInterval: -1, // disable watcher
		OnEvent: func(e *output.Event) {
			mu.Lock()
			events = append(events, e)
			mu.Unlock()
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		_ = r.Run(ctx)
		close(runDone)
	}()
	if !waitForEvent(t, &mu, &events, "logical.runner", "started", 1*time.Second) {
		t.Fatal("never saw started event")
	}
	// Add a stream — without the watcher, no stream.added should
	// fire.
	if _, err := mgr.Add(logical.AddOptions{
		Name:        "ignored",
		Deployment:  "no-conn",
		Slot:        "s2",
		Plugin:      "pgoutput",
		Publication: "pub",
		SinkKind:    "chunked",
		RepoURL:     "file:///tmp/whatever",
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	mu.Lock()
	for _, e := range events {
		if e.Component == "logical.runner" && e.Op == "stream.added" {
			mu.Unlock()
			t.Fatal("watcher should be disabled but stream.added fired")
		}
	}
	mu.Unlock()

	cancel()
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Run didn't exit")
	}
}

// silence-unused for filepath import.
var _ = filepath.Join

// waitForEvent polls events for one matching component+op until
// timeout. Locks mu while reading events.
func waitForEvent(t *testing.T, mu *sync.Mutex, events *[]*output.Event, component, op string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		mu.Lock()
		for _, e := range *events {
			if e.Component == component && e.Op == op {
				mu.Unlock()
				return true
			}
		}
		mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
