package follower_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/patroni"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/replication"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/follower"
)

// patroniClusterServer is a tiny httptest server that returns a
// fixed /cluster response. Used to exercise the
// SlotRoleReplica path that calls c.opts.Client.Cluster(ctx).
type patroniClusterServer struct {
	*httptest.Server
	mu      sync.Mutex
	cluster any
}

func newClusterServer(t *testing.T, initial any) *patroniClusterServer {
	t.Helper()
	s := &patroniClusterServer{cluster: initial}
	mux := http.NewServeMux()
	mux.HandleFunc("/cluster", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		body := s.cluster
		s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(body)
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Server.Close)
	return s
}

// TestNew_RejectsBothSlotNameAndSlots: mutually-exclusive.
func TestNew_RejectsBothSlotNameAndSlots(t *testing.T) {
	_, err := follower.New(follower.Options{
		Client:        fakePatroniClient(t),
		Deployment:    "db1",
		DSNFor:        func(string, int) string { return "x" },
		TimelineStore: newTimelineStore(t),
		SlotName:      "slot1",
		Slots: []follower.SlotSpec{
			{Name: "slot1", Role: follower.SlotRoleLeader},
		},
	})
	if err == nil {
		t.Fatal("expected error for both SlotName + Slots set")
	}
}

// TestNew_RejectsNeitherSlotConfig: must provide one of them.
func TestNew_RejectsNeitherSlotConfig(t *testing.T) {
	_, err := follower.New(follower.Options{
		Client:        fakePatroniClient(t),
		Deployment:    "db1",
		DSNFor:        func(string, int) string { return "x" },
		TimelineStore: newTimelineStore(t),
	})
	if err == nil {
		t.Fatal("expected error when neither SlotName nor Slots is set")
	}
}

// TestNew_NormalisesSlotNameToSingleEntrySlots: the v0.5
// single-slot mode gets transparently turned into Slots with
// one leader-pinned entry. Existing single-slot tests keep
// working without changes.
func TestNew_NormalisesSlotNameToSingleEntrySlots(t *testing.T) {
	// We can't directly inspect the normalised Slots field
	// (Coordinator hides it). Instead we drive a reconcile
	// and observe the slot_name in the resulting event body.
	rec := &recordingSink{}
	coord, _ := follower.New(follower.Options{
		Client:        fakePatroniClient(t),
		Deployment:    "db1",
		DSNFor:        func(string, int) string { return "postgres://x" },
		TimelineStore: newTimelineStore(t),
		SlotName:      "single_slot",
		OnEvent:       rec.record,
		ReconcileSlot: func(context.Context, string) (*replication.SlotContinuityResult, error) {
			return &replication.SlotContinuityResult{
				Outcome: replication.SlotFound,
				Slot:    &replication.SlotInfo{Name: "single_slot"},
			}, nil
		},
		CaptureTimelineHistory: func(context.Context, string, uint32) error { return nil },
	})
	coord.HandleLeaderChange(context.Background(), patroni.LeaderChange{
		New: &patroni.LeaderEndpoint{Name: "node-1", Host: "h1", Port: 5432, Timeline: 2, Role: "leader"},
	})
	ev := rec.firstWithOp("slot_reconciled")
	if ev == nil {
		t.Fatalf("expected slot_reconciled event; ops=%v", rec.ops())
	}
	body, _ := ev.Body.(map[string]any)
	if body["slot_name"] != "single_slot" {
		t.Errorf("body.slot_name = %v, want 'single_slot' (legacy SlotName)", body["slot_name"])
	}
	if body["slot_role"] != "leader" {
		t.Errorf("body.slot_role = %v, want 'leader' (single-slot default)", body["slot_role"])
	}
}

// TestNew_RejectsDuplicateSlotName: two slots with the same
// name is a config bug — the underlying replication.EnsureSlot
// would race on the same PG slot from two reconcile paths.
func TestNew_RejectsDuplicateSlotName(t *testing.T) {
	_, err := follower.New(follower.Options{
		Client:        fakePatroniClient(t),
		Deployment:    "db1",
		DSNFor:        func(string, int) string { return "x" },
		TimelineStore: newTimelineStore(t),
		Slots: []follower.SlotSpec{
			{Name: "duplicate", Role: follower.SlotRoleLeader},
			{Name: "duplicate", Role: follower.SlotRoleReplica},
		},
	})
	if err == nil {
		t.Fatal("expected error for duplicate slot name")
	}
}

// TestNew_RejectsBadSlotRole: only "leader" or "replica" allowed.
func TestNew_RejectsBadSlotRole(t *testing.T) {
	_, err := follower.New(follower.Options{
		Client:        fakePatroniClient(t),
		Deployment:    "db1",
		DSNFor:        func(string, int) string { return "x" },
		TimelineStore: newTimelineStore(t),
		Slots: []follower.SlotSpec{
			{Name: "slot1", Role: follower.SlotRole("sync_standby")},
		},
	})
	if err == nil {
		t.Fatal("expected error for bad slot role")
	}
}

// TestHandleLeaderChange_MultiSlotReconcilesEachSlot: a
// dual-slot config triggers TWO slot_reconciled events per
// leader change, each tagged with its own slot_name + slot_role.
// Validates the Mechanism 3 happy path.
func TestHandleLeaderChange_MultiSlotReconcilesEachSlot(t *testing.T) {
	srv := newClusterServer(t, map[string]any{
		"members": []any{
			map[string]any{
				"name": "node-1", "role": "leader", "state": "running",
				"host": "node-1.example", "port": 5432, "timeline": 7,
			},
			map[string]any{
				"name": "node-2", "role": "replica", "state": "running",
				"host": "node-2.example", "port": 5432, "timeline": 7,
			},
		},
	})
	client, err := patroni.NewClient(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	rec := &recordingSink{}
	var reconcileSlotCalls int
	coord, err := follower.New(follower.Options{
		Client:        client,
		Deployment:    "db1",
		DSNFor:        func(host string, port int) string { return "postgres://" + host },
		TimelineStore: newTimelineStore(t),
		Slots: []follower.SlotSpec{
			{Name: "pg_hardstorage_db1_primary", Role: follower.SlotRoleLeader},
			{Name: "pg_hardstorage_db1_replica", Role: follower.SlotRoleReplica},
		},
		OnEvent: rec.record,
		ReconcileSlot: func(_ context.Context, dsn string) (*replication.SlotContinuityResult, error) {
			reconcileSlotCalls++
			return &replication.SlotContinuityResult{
				Outcome: replication.SlotFound,
				Slot:    &replication.SlotInfo{},
			}, nil
		},
		CaptureTimelineHistory: func(context.Context, string, uint32) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}

	coord.HandleLeaderChange(context.Background(), patroni.LeaderChange{
		New: &patroni.LeaderEndpoint{Name: "node-1", Host: "node-1.example", Port: 5432, Timeline: 7, Role: "leader"},
	})

	if reconcileSlotCalls != 2 {
		t.Errorf("ReconcileSlot called %d times, want 2 (one per slot)", reconcileSlotCalls)
	}
	// Two slot_reconciled events, one per slot.
	rec.mu.Lock()
	defer rec.mu.Unlock()
	var reconciledSlots []string
	var reconciledRoles []string
	for _, ev := range rec.events {
		if ev.Op == "slot_reconciled" {
			body, _ := ev.Body.(map[string]any)
			reconciledSlots = append(reconciledSlots, body["slot_name"].(string))
			reconciledRoles = append(reconciledRoles, body["slot_role"].(string))
		}
	}
	if len(reconciledSlots) != 2 {
		t.Fatalf("got %d slot_reconciled events, want 2", len(reconciledSlots))
	}
	for _, want := range []string{"pg_hardstorage_db1_primary", "pg_hardstorage_db1_replica"} {
		found := false
		for _, got := range reconciledSlots {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing slot %q in reconciled set %v", want, reconciledSlots)
		}
	}
	for _, want := range []string{"leader", "replica"} {
		found := false
		for _, got := range reconciledRoles {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing role %q in reconciled set %v", want, reconciledRoles)
		}
	}
}

// TestHandleLeaderChange_NoRunningReplicaEmitsWarning: when no
// running replica exists in /cluster (single-node clusters or
// replica down), the replica-pinned slot reconcile is skipped
// with a structured warning. The leader-pinned slot still
// reconciles.
func TestHandleLeaderChange_NoRunningReplicaEmitsWarning(t *testing.T) {
	srv := newClusterServer(t, map[string]any{
		"members": []any{
			map[string]any{
				"name": "node-1", "role": "leader", "state": "running",
				"host": "node-1.example", "port": 5432, "timeline": 1,
			},
			// node-2 listed but state=stopped → ineligible.
			map[string]any{
				"name": "node-2", "role": "replica", "state": "stopped",
				"host": "node-2.example", "port": 5432, "timeline": 1,
			},
		},
	})
	client, _ := patroni.NewClient(srv.URL)

	rec := &recordingSink{}
	var reconcileSlotCalls int
	coord, _ := follower.New(follower.Options{
		Client:        client,
		Deployment:    "db1",
		DSNFor:        func(host string, port int) string { return "postgres://" + host },
		TimelineStore: newTimelineStore(t),
		Slots: []follower.SlotSpec{
			{Name: "primary_slot", Role: follower.SlotRoleLeader},
			{Name: "replica_slot", Role: follower.SlotRoleReplica},
		},
		OnEvent: rec.record,
		ReconcileSlot: func(context.Context, string) (*replication.SlotContinuityResult, error) {
			reconcileSlotCalls++
			return &replication.SlotContinuityResult{
				Outcome: replication.SlotFound,
				Slot:    &replication.SlotInfo{},
			}, nil
		},
		CaptureTimelineHistory: func(context.Context, string, uint32) error { return nil },
	})
	coord.HandleLeaderChange(context.Background(), patroni.LeaderChange{
		New: &patroni.LeaderEndpoint{Name: "node-1", Host: "node-1.example", Port: 5432, Timeline: 1, Role: "leader"},
	})

	if reconcileSlotCalls != 1 {
		t.Errorf("ReconcileSlot called %d times, want 1 (only the leader-slot)", reconcileSlotCalls)
	}
	if rec.firstWithOp("no_replica_available") == nil {
		t.Errorf("expected no_replica_available event; got ops=%v", rec.ops())
	}
}

// TestHandleLeaderChange_ClusterFetchFailureEmitsWarning: when
// the /cluster fetch fails for a SlotRoleReplica resolve, the
// replica slot is skipped with a cluster_fetch_failed warning;
// other slots still reconcile.
func TestHandleLeaderChange_ClusterFetchFailureEmitsWarning(t *testing.T) {
	// Patroni server that always 500s.
	mux := http.NewServeMux()
	mux.HandleFunc("/cluster", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	client, _ := patroni.NewClient(srv.URL)

	rec := &recordingSink{}
	coord, _ := follower.New(follower.Options{
		Client:        client,
		Deployment:    "db1",
		DSNFor:        func(host string, port int) string { return "postgres://" + host },
		TimelineStore: newTimelineStore(t),
		Slots: []follower.SlotSpec{
			{Name: "primary_slot", Role: follower.SlotRoleLeader},
			{Name: "replica_slot", Role: follower.SlotRoleReplica},
		},
		OnEvent: rec.record,
		ReconcileSlot: func(context.Context, string) (*replication.SlotContinuityResult, error) {
			return &replication.SlotContinuityResult{
				Outcome: replication.SlotFound,
				Slot:    &replication.SlotInfo{},
			}, nil
		},
		CaptureTimelineHistory: func(context.Context, string, uint32) error { return nil },
	})
	coord.HandleLeaderChange(context.Background(), patroni.LeaderChange{
		New: &patroni.LeaderEndpoint{Name: "node-1", Host: "node-1.example", Port: 5432, Timeline: 1, Role: "leader"},
	})

	if rec.firstWithOp("cluster_fetch_failed") == nil {
		t.Errorf("expected cluster_fetch_failed event; got ops=%v", rec.ops())
	}
	// Leader slot should still reconcile.
	if rec.firstWithOp("slot_reconciled") == nil {
		t.Errorf("expected slot_reconciled (leader); got ops=%v", rec.ops())
	}
}

// stubOutputEventBody pulls .Body as map for assertion convenience.
// (Used implicitly above; we assert via type-asserts inline.)
var _ = func() *output.Event { return nil }()
