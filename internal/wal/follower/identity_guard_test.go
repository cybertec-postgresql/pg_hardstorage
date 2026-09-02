package follower_test

// identity_guard_test.go — the cluster-identity defence was armed only
// in its own unit test.
//
// patroni.FollowerOptions.ExpectedSystemID makes the follower refuse to
// follow a leader whose cluster reports a different system_identifier:
// "misconfigured fleet — refusing to follow a different cluster's
// leader". internal/patroni has had that logic, and a test for it,
// since audit #24, whose commit message even records that the field had
// previously been "stored but never read".
//
// It was still never WRITTEN. A tree-wide search for ExpectedSystemID
// found it in exactly one place outside the follower itself:
// internal/patroni/follower_test.go. Coordinator.Run built its
// FollowerOptions with Client, Interval, OnEvent and OnPollError and
// nothing else, and follower.Options had no field to carry it, so end
// to end the defence never ran.
//
// What that costs, if an agent is pointed at the wrong Patroni
// endpoint: it follows that cluster's leader, reconciles THIS
// deployment's replication slots against it, and persists WAL-gap
// records computed from a foreign cluster's restart_lsn into
// wal/<deployment>/gaps/. Those records are then consulted by the PITR
// pre-flight, which refuses restores over ranges that were never gaps
// for this deployment.
//
// This test pins the wiring: the value the Coordinator is given must
// reach the follower.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/patroni"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/follower"
)

func TestOptions_CarriesExpectedSystemID(t *testing.T) {
	const want = "7000000000000000001"
	opts := follower.Options{
		Client:           fakePatroniClient(t),
		Deployment:       "db1",
		DSNFor:           func(string, int) string { return "postgres://x" },
		TimelineStore:    newTimelineStore(t),
		SlotName:         "slot1",
		ExpectedSystemID: want,
	}
	c, err := follower.New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := follower.ExpectedSystemIDOf(c); got != want {
		t.Fatalf("ExpectedSystemID = %q, want %q.\n\nThe field must survive New and reach "+
			"patroni.Start; the defence it arms was dead in production precisely because "+
			"nothing carried the value that far.", got, want)
	}
}

// An empty value must stay empty — that is the honest "cannot determine
// the identifier" state, and silently substituting anything would arm
// the guard against the wrong cluster.
func TestOptions_EmptyExpectedSystemIDStaysEmpty(t *testing.T) {
	c, err := follower.New(follower.Options{
		Client:        fakePatroniClient(t),
		Deployment:    "db1",
		DSNFor:        func(string, int) string { return "postgres://x" },
		TimelineStore: newTimelineStore(t),
		SlotName:      "slot1",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := follower.ExpectedSystemIDOf(c); got != "" {
		t.Errorf("ExpectedSystemID = %q for an unset option; the guard must stay inactive "+
			"rather than lock onto a fabricated identifier", got)
	}
}

// The wiring test above proves the value survives New. It does NOT
// prove it reaches patroni.Start — and that is the half that was
// broken, so a mutation removing the field from the Start call passed
// it cleanly. This drives the whole Coordinator against a fake Patroni
// endpoint reporting a FOREIGN system_identifier and asserts the
// follower actually disables and says so.
//
// It covers both halves of the fix: the plumbing, and the
// follower_disabled event that stops a permanently-disabled follower
// looking like a transient poll blip that recovered.
func TestRun_ForeignClusterDisablesTheFollowerAndReportsIt(t *testing.T) {
	const ours = "7000000000000000001"
	const theirs = "9999999999999999999"

	srv := newClusterServer(t, map[string]any{
		"system_identifier": theirs,
		"members": []map[string]any{
			{"name": "n1", "role": "leader", "state": "running",
				"host": "127.0.0.1", "port": 5432, "timeline": 1},
		},
	})
	client, err := patroni.NewClient(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	var (
		mu     sync.Mutex
		events []*output.Event
	)
	c, err := follower.New(follower.Options{
		Client:           client,
		Deployment:       "db1",
		DSNFor:           func(string, int) string { return "postgres://x" },
		TimelineStore:    newTimelineStore(t),
		SlotName:         "slot1",
		Interval:         20 * time.Millisecond,
		ExpectedSystemID: ours,
		OnEvent: func(ev *output.Event) {
			mu.Lock()
			events = append(events, ev)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	deadline := time.After(4 * time.Second)
	for {
		mu.Lock()
		var sawDisabled, sawLeader bool
		for _, ev := range events {
			switch ev.Op {
			case "follower_disabled":
				sawDisabled = true
			case "leader_changed", "slot_reconciled":
				sawLeader = true
			}
		}
		mu.Unlock()
		if sawLeader {
			t.Fatal("the coordinator acted on a FOREIGN cluster's leader.\n\n" +
				"ExpectedSystemID was set, the endpoint reports a different " +
				"system_identifier, and the follower should have refused: reconciling this " +
				"deployment's slots against another cluster writes WAL-gap records from " +
				"its LSNs into our gapstate, which the PITR pre-flight then honours.")
		}
		if sawDisabled {
			cancel()
			<-done
			return
		}
		select {
		case <-deadline:
			mu.Lock()
			ops := make([]string, 0, len(events))
			for _, ev := range events {
				ops = append(ops, ev.Op)
			}
			mu.Unlock()
			t.Fatalf("no follower_disabled event after 4s.\n\nA permanently disabled "+
				"follower is reported only through OnPollError as patroni_poll_failed at "+
				"WARNING — the same event as one failed HTTP call — and then never fires "+
				"again, so it reads as a blip that recovered. Events seen: %v", ops)
		case <-time.After(25 * time.Millisecond):
		}
	}
}
