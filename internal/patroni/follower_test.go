package patroni_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/patroni"
)

// fakeFollowSrv is a tiny /cluster server whose response can be
// swapped at runtime so tests can simulate a leader change.
type fakeFollowSrv struct {
	*httptest.Server
	mu      sync.Mutex
	cluster any
	queries atomic.Int64
}

func newFakeFollowSrv(t *testing.T, initial any) *fakeFollowSrv {
	t.Helper()
	f := &fakeFollowSrv{cluster: initial}
	mux := http.NewServeMux()
	mux.HandleFunc("/cluster", func(w http.ResponseWriter, r *http.Request) {
		f.queries.Add(1)
		f.mu.Lock()
		body := f.cluster
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Server.Close)
	return f
}

func (f *fakeFollowSrv) setCluster(c any) {
	f.mu.Lock()
	f.cluster = c
	f.mu.Unlock()
}

// clusterWithLeader builds a /cluster response with the named
// member as the leader.
func clusterWithLeader(leaderName, host string, port int, tli uint32) map[string]any {
	return map[string]any{
		"scope": "test",
		"members": []any{
			map[string]any{
				"name":     leaderName,
				"role":     "leader",
				"state":    "running",
				"host":     host,
				"port":     port,
				"timeline": tli,
			},
		},
	}
}

// TestFollower_FirstPollObservesInitialLeader: the constructor
// fires one inline poll so callers can read the initial leader
// without having to wait for a tick. We verify by waiting for the
// OnEvent callback to fire (it runs synchronously on the poll
// goroutine; the test blocks on a channel for determinism).
func TestFollower_FirstPollObservesInitialLeader(t *testing.T) {
	srv := newFakeFollowSrv(t, clusterWithLeader("node-1", "host-1", 5432, 1))

	c, err := patroni.NewClient(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	events := make(chan patroni.LeaderChange, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f, err := patroni.Start(ctx, patroni.FollowerOptions{
		Client:   c,
		Interval: 50 * time.Millisecond,
		OnEvent:  func(ev patroni.LeaderChange) { events <- ev },
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	select {
	case ev := <-events:
		if ev.Old != nil {
			t.Errorf("first event Old should be nil, got %+v", ev.Old)
		}
		if ev.New == nil || ev.New.Name != "node-1" {
			t.Errorf("first event New = %+v, want node-1", ev.New)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first OnEvent")
	}

	leader := f.GetLeader()
	if leader == nil || leader.Host != "host-1" || leader.Port != 5432 {
		t.Errorf("GetLeader = %+v, want host-1:5432", leader)
	}
}

// TestFollower_DetectsLeaderChange: the canonical Patroni
// failover path. node-1 starts as leader; we promote node-2 by
// swapping the /cluster response; the follower's next poll emits
// a LeaderChange with Old=node-1 / New=node-2.
func TestFollower_DetectsLeaderChange(t *testing.T) {
	srv := newFakeFollowSrv(t, clusterWithLeader("node-1", "host-1", 5432, 1))

	c, err := patroni.NewClient(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	events := make(chan patroni.LeaderChange, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := patroni.Start(ctx, patroni.FollowerOptions{
		Client:   c,
		Interval: 50 * time.Millisecond,
		OnEvent:  func(ev patroni.LeaderChange) { events <- ev },
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Drain the initial event (node-1 first observation).
	select {
	case ev := <-events:
		if ev.New == nil || ev.New.Name != "node-1" {
			t.Errorf("initial event = %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("missing initial event")
	}

	// Failover: node-2 is now leader, on TLI 2.
	srv.setCluster(clusterWithLeader("node-2", "host-2", 5432, 2))

	select {
	case ev := <-events:
		if ev.Old == nil || ev.Old.Name != "node-1" {
			t.Errorf("change event Old = %+v, want node-1", ev.Old)
		}
		if ev.New == nil || ev.New.Name != "node-2" || ev.New.Timeline != 2 {
			t.Errorf("change event New = %+v, want node-2 TLI 2", ev.New)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for leader-change event")
	}
}

// TestFollower_NoChangeNoSpamEvents: a stable cluster shouldn't
// emit duplicate events on every poll. The first poll fires once;
// subsequent polls with identical bytes are silent.
func TestFollower_NoChangeNoSpamEvents(t *testing.T) {
	srv := newFakeFollowSrv(t, clusterWithLeader("node-1", "host-1", 5432, 1))

	c, err := patroni.NewClient(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	var events atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := patroni.Start(ctx, patroni.FollowerOptions{
		Client:   c,
		Interval: 25 * time.Millisecond,
		OnEvent:  func(patroni.LeaderChange) { events.Add(1) },
	}); err != nil {
		t.Fatal(err)
	}

	// Let the follower run for a while (~10 polls).
	time.Sleep(300 * time.Millisecond)
	got := events.Load()
	if got != 1 {
		t.Errorf("OnEvent fired %d times for stable cluster; want exactly 1", got)
	}
	// Confirm we did poll repeatedly (sanity that the test isn't
	// just measuring an idle goroutine).
	if q := srv.queries.Load(); q < 5 {
		t.Errorf("expected ≥ 5 polls, got %d", q)
	}
}

// TestFollower_LeaderGoneEmitsNilNew: when /cluster has no member
// with role=leader (DCS lock between holders), the follower
// emits New=nil so the WAL streaming loop knows to pause.
func TestFollower_LeaderGoneEmitsNilNew(t *testing.T) {
	srv := newFakeFollowSrv(t, clusterWithLeader("node-1", "host-1", 5432, 1))

	c, err := patroni.NewClient(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	events := make(chan patroni.LeaderChange, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := patroni.Start(ctx, patroni.FollowerOptions{
		Client:   c,
		Interval: 25 * time.Millisecond,
		OnEvent:  func(ev patroni.LeaderChange) { events <- ev },
	}); err != nil {
		t.Fatal(err)
	}

	// Drain initial.
	<-events

	// Strip the leader role.
	srv.setCluster(map[string]any{
		"scope": "test",
		"members": []any{
			map[string]any{
				"name": "node-1", "role": "replica", "state": "running",
				"host": "host-1", "port": 5432, "timeline": 1,
			},
		},
	})

	select {
	case ev := <-events:
		if ev.Old == nil {
			t.Errorf("Old should be the previous leader; got nil")
		}
		if ev.New != nil {
			t.Errorf("New should be nil when no member is leader; got %+v", ev.New)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for no-leader event")
	}
}

// TestFollower_OnPollErrorFiresOnUnreachable: a server returning
// 500 surfaces via the OnPollError callback. The follower keeps
// polling so a transient outage doesn't terminate the loop.
func TestFollower_OnPollErrorFiresOnUnreachable(t *testing.T) {
	mux := http.NewServeMux()
	var down atomic.Bool
	mux.HandleFunc("/cluster", func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(clusterWithLeader("node-1", "host-1", 5432, 1))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c, err := patroni.NewClient(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	pollErrs := make(chan error, 8)
	events := make(chan patroni.LeaderChange, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := patroni.Start(ctx, patroni.FollowerOptions{
		Client:      c,
		Interval:    25 * time.Millisecond,
		OnEvent:     func(ev patroni.LeaderChange) { events <- ev },
		OnPollError: func(e error) { pollErrs <- e },
	}); err != nil {
		t.Fatal(err)
	}

	// Drain initial success.
	<-events

	// Flip the server to error mode.
	down.Store(true)

	select {
	case <-pollErrs:
		// Got the error.
	case <-time.After(2 * time.Second):
		t.Fatal("expected OnPollError to fire after server flip")
	}

	// Recover.
	down.Store(false)
	// We should NOT see a leader change (same leader, just a
	// transient outage). But the follower should still be alive.
	select {
	case ev := <-events:
		t.Errorf("unexpected leader change after recovery: %+v", ev)
	case <-time.After(200 * time.Millisecond):
		// Expected: no event.
	}
}

// TestFollower_DisableStopsEmittingChanges: an operator-driven
// Disable is the kill-switch; subsequent polls don't emit events
// and DisabledReason surfaces the supplied string.
func TestFollower_DisableStopsEmittingChanges(t *testing.T) {
	srv := newFakeFollowSrv(t, clusterWithLeader("node-1", "host-1", 5432, 1))

	c, err := patroni.NewClient(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	events := make(chan patroni.LeaderChange, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f, err := patroni.Start(ctx, patroni.FollowerOptions{
		Client:   c,
		Interval: 25 * time.Millisecond,
		OnEvent:  func(ev patroni.LeaderChange) { events <- ev },
	})
	if err != nil {
		t.Fatal(err)
	}

	<-events // drain initial

	f.Disable("operator-initiated")
	srv.setCluster(clusterWithLeader("node-2", "host-2", 5432, 2))

	select {
	case ev := <-events:
		t.Errorf("disable should have suppressed event; got %+v", ev)
	case <-time.After(200 * time.Millisecond):
		// Expected: no event.
	}
	r, disabled := f.DisabledReason()
	if !disabled || r != "operator-initiated" {
		t.Errorf("DisabledReason = (%q, %v), want (operator-initiated, true)", r, disabled)
	}
}

// TestFollower_DoneClosesOnContextCancel: cancelling the parent
// context shuts the loop down and Done is closed. Required for
// orderly shutdown alongside the agent.
func TestFollower_DoneClosesOnContextCancel(t *testing.T) {
	srv := newFakeFollowSrv(t, clusterWithLeader("node-1", "host-1", 5432, 1))

	c, err := patroni.NewClient(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	f, err := patroni.Start(ctx, patroni.FollowerOptions{
		Client:   c,
		Interval: 25 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	cancel()
	select {
	case <-f.Done():
		// Expected.
	case <-time.After(2 * time.Second):
		t.Fatal("Done channel did not close after context cancel")
	}
}

// TestFollower_SystemIDMismatchDisables reproduces bug #24: with
// ExpectedSystemID set, an observed cluster system identifier that
// differs must DISABLE the follower (stop emitting leaders) and surface
// a critical error. Previously ExpectedSystemID was stored but never
// read — the documented cluster-identity defence was dead code.
func TestFollower_SystemIDMismatchDisables(t *testing.T) {
	srv := newFakeFollowSrv(t, clusterWithLeader("node-1", "host-1", 5432, 1))

	c, err := patroni.NewClient(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	pollErrs := make(chan error, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f, err := patroni.Start(ctx, patroni.FollowerOptions{
		Client:           c,
		Interval:         25 * time.Millisecond,
		ExpectedSystemID: "7000000000000000001",
		OnEvent:          func(patroni.LeaderChange) {},
		OnPollError:      func(e error) { pollErrs <- e },
	})
	if err != nil {
		t.Fatal(err)
	}
	// Observed cluster reports a DIFFERENT system identifier.
	f.SetSystemIDProbe(func(context.Context) (string, bool, error) {
		return "9999999999999999999", true, nil
	})

	select {
	case e := <-pollErrs:
		if e == nil || !contains(e.Error(), "system-identifier mismatch") {
			t.Fatalf("expected system-identifier mismatch error; got %v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for mismatch poll error")
	}

	// The follower must now be disabled.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, disabled := f.DisabledReason(); disabled {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("follower was not disabled after system-id mismatch")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestFollower_SystemIDMatchKeepsFollowing: when the observed system
// identifier matches ExpectedSystemID, the follower behaves normally and
// emits the leader.
func TestFollower_SystemIDMatchKeepsFollowing(t *testing.T) {
	srv := newFakeFollowSrv(t, clusterWithLeader("node-1", "host-1", 5432, 1))

	c, err := patroni.NewClient(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	events := make(chan patroni.LeaderChange, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f, err := patroni.Start(ctx, patroni.FollowerOptions{
		Client:           c,
		Interval:         25 * time.Millisecond,
		ExpectedSystemID: "7000000000000000001",
		OnEvent:          func(ev patroni.LeaderChange) { events <- ev },
	})
	if err != nil {
		t.Fatal(err)
	}
	f.SetSystemIDProbe(func(context.Context) (string, bool, error) {
		return "7000000000000000001", true, nil
	})

	select {
	case ev := <-events:
		if ev.New == nil || ev.New.Name != "node-1" {
			t.Errorf("expected node-1 leader; got %+v", ev.New)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for leader event under matching system id")
	}
	if _, disabled := f.DisabledReason(); disabled {
		t.Fatal("follower should not be disabled when system id matches")
	}
}

// TestFollower_SystemIDAbsentKeepsFollowing: when Patroni doesn't
// surface a system_identifier (older versions), the follower can't
// verify and must keep working rather than blocking on the missing
// field.
func TestFollower_SystemIDAbsentKeepsFollowing(t *testing.T) {
	srv := newFakeFollowSrv(t, clusterWithLeader("node-1", "host-1", 5432, 1))

	c, err := patroni.NewClient(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	events := make(chan patroni.LeaderChange, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f, err := patroni.Start(ctx, patroni.FollowerOptions{
		Client:           c,
		Interval:         25 * time.Millisecond,
		ExpectedSystemID: "7000000000000000001",
		OnEvent:          func(ev patroni.LeaderChange) { events <- ev },
	})
	if err != nil {
		t.Fatal(err)
	}
	// ok=false: field absent.
	f.SetSystemIDProbe(func(context.Context) (string, bool, error) {
		return "", false, nil
	})

	select {
	case ev := <-events:
		if ev.New == nil || ev.New.Name != "node-1" {
			t.Errorf("expected node-1 leader; got %+v", ev.New)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for leader event when system id absent")
	}
	if _, disabled := f.DisabledReason(); disabled {
		t.Fatal("follower should not disable when system id is unavailable")
	}
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}

// TestFollower_RejectsNilClient: the constructor surfaces a clear
// error rather than panicking later when a poll fires.
func TestFollower_RejectsNilClient(t *testing.T) {
	if _, err := patroni.Start(context.Background(), patroni.FollowerOptions{}); err == nil {
		t.Error("expected error for nil Client")
	}
}

// TestFollower_TimelineChangeOnSameNodeIsAChange: a leader stays
// on the same node but its TLI advances (e.g. the operator manually
// promoted then re-promoted the same node, or PG bounced and
// landed on a fresh TLI). The follower must still emit a change
// so the WAL streaming loop captures the new TIMELINE_HISTORY.
func TestFollower_TimelineChangeOnSameNodeIsAChange(t *testing.T) {
	srv := newFakeFollowSrv(t, clusterWithLeader("node-1", "host-1", 5432, 1))

	c, err := patroni.NewClient(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	events := make(chan patroni.LeaderChange, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := patroni.Start(ctx, patroni.FollowerOptions{
		Client:   c,
		Interval: 25 * time.Millisecond,
		OnEvent:  func(ev patroni.LeaderChange) { events <- ev },
	}); err != nil {
		t.Fatal(err)
	}

	<-events // drain initial

	// Same node, new TLI.
	srv.setCluster(clusterWithLeader("node-1", "host-1", 5432, 5))

	select {
	case ev := <-events:
		if ev.New == nil || ev.New.Timeline != 5 {
			t.Errorf("expected New.Timeline=5; got %+v", ev.New)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeline change should fire an event")
	}
}
