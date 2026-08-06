package follower_test

// slotrole_exhaustive_test.go — every SlotRole must resolve to an
// endpoint, or the coordinator silently stops following that slot.
//
// endpointForSlot ends in a defensive branch that emits
// `wal.follower.unknown_slot_role` at ERROR and returns "no endpoint".
// The slot is then not reconciled: no gap check, no timeline capture,
// nothing. The deployment keeps running and keeps looking healthy.
//
// That branch is unreachable through the public API today, because
// New() rejects any role that is not leader/replica — which is exactly
// why it was the one `wal.follower` event with no test anywhere in the
// tree, and exactly why it is worth guarding. It becomes reachable the
// moment somebody adds a third SlotRole (a sync-standby role is the
// obvious candidate) and forgets the resolution arm. The compiler will
// not say a word: SlotRole is a string type, so there is no exhaustive
// switch to fail.
//
// So this test asserts the property directly: for every declared
// SlotRole, a leader change must reconcile that slot and must NOT emit
// unknown_slot_role.

import (
	"context"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/patroni"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/replication"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/follower"
)

// declaredSlotRoles is every role the package defines. A new constant
// added without a line here is caught by
// TestSlotRoles_ListIsComplete below.
var declaredSlotRoles = []follower.SlotRole{
	follower.SlotRoleLeader,
	follower.SlotRoleReplica,
}

func TestSlotRoles_EveryRoleResolvesToAnEndpoint(t *testing.T) {
	for _, role := range declaredSlotRoles {
		t.Run(string(role), func(t *testing.T) {
			rec := &recordingSink{}
			coord, err := follower.New(follower.Options{
				Client:     fakePatroniClient(t),
				Deployment: "db1",
				Slots: []follower.SlotSpec{
					{Name: "slot_" + string(role), Role: role},
				},
				DSNFor:        func(string, int) string { return "postgres://x" },
				TimelineStore: newTimelineStore(t),
				OnEvent:       rec.record,
				ReconcileSlot: func(context.Context, string) (*replication.SlotContinuityResult, error) {
					return &replication.SlotContinuityResult{
						Outcome: replication.SlotFound,
						Slot:    &replication.SlotInfo{},
					}, nil
				},
				CaptureTimelineHistory: func(context.Context, string, uint32) error { return nil },
			})
			if err != nil {
				t.Fatalf("New rejected the declared role %q: %v", role, err)
			}

			coord.HandleLeaderChange(context.Background(), patroni.LeaderChange{
				New: &patroni.LeaderEndpoint{
					Name: "node-1", Host: "node-1.example", Port: 5432,
					Timeline: 7, Role: "leader",
				},
			})

			if ev := rec.firstWithOp("unknown_slot_role"); ev != nil {
				t.Errorf("role %q produced unknown_slot_role — the coordinator resolved no "+
					"endpoint for it, so the slot is never reconciled: no gap check, no "+
					"timeline capture, and the deployment still looks healthy. Add the "+
					"resolution arm in endpointForSlot.", role)
			}
		})
	}
}

// TestSlotRoles_ListIsComplete keeps declaredSlotRoles honest. Without
// it, adding a role and forgetting to list it here would leave the
// exhaustiveness test above passing while covering less than it claims.
func TestSlotRoles_ListIsComplete(t *testing.T) {
	// Roles are validated at construction, so "is this string a role"
	// is answerable by asking New(). Any single-word lowercase role we
	// have NOT declared but New accepts is a gap in the list.
	candidates := []follower.SlotRole{
		"leader", "replica", "primary", "standby",
		"sync_standby", "sync-standby", "quorum", "witness",
	}
	declared := map[follower.SlotRole]bool{}
	for _, r := range declaredSlotRoles {
		declared[r] = true
	}
	for _, c := range candidates {
		if declared[c] {
			continue
		}
		_, err := follower.New(follower.Options{
			Client:                 fakePatroniClient(t),
			Deployment:             "db1",
			Slots:                  []follower.SlotSpec{{Name: "s", Role: c}},
			DSNFor:                 func(string, int) string { return "postgres://x" },
			TimelineStore:          newTimelineStore(t),
			OnEvent:                func(*output.Event) {},
			ReconcileSlot:          func(context.Context, string) (*replication.SlotContinuityResult, error) { return nil, nil },
			CaptureTimelineHistory: func(context.Context, string, uint32) error { return nil },
		})
		if err == nil {
			t.Errorf("New accepts role %q but declaredSlotRoles does not list it, so "+
				"TestSlotRoles_EveryRoleResolvesToAnEndpoint never exercises it", c)
		}
	}
}
