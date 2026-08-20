package server_test

// agents_prune_test.go — memory-leak audit #3: the agent registry
// grew by one entry per distinct agent ID for the process lifetime
// (List's active filter only hides entries, it never deletes), so
// decommissioned or crashed hosts accumulated forever and the
// /metrics 'total' counted them as zombies.

import (
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/server"
)

// TestAgentRegistry_HeartbeatPrunesStaleEntries pins the opportunistic
// prune: an agent silent past pruneStaleFactor × timeout is dropped by
// the next heartbeat's sweep, while a recently heartbeating agent
// survives the same sweep.
func TestAgentRegistry_HeartbeatPrunesStaleEntries(t *testing.T) {
	// 20 ms inactivity timeout ⇒ prune cutoff 200 ms.
	reg := server.NewAgentRegistry(20 * time.Millisecond)

	for _, id := range []string{"agent-alive", "agent-dead"} {
		if _, err := reg.Heartbeat(server.HeartbeatRequest{ID: id, Host: id + ".host"}); err != nil {
			t.Fatalf("Heartbeat(%s): %v", id, err)
		}
	}
	if got := len(reg.List(true)); got != 2 {
		t.Fatalf("List(true) = %d agents, want 2 before the sweep", got)
	}

	// Let agent-dead cross the prune cutoff (10 × 20 ms = 200 ms);
	// 250 ms leaves margin on the sleep.
	time.Sleep(250 * time.Millisecond)

	// agent-alive heartbeats again: its entry is fresh, so the sweep
	// triggered by this heartbeat must keep it and drop agent-dead.
	if _, err := reg.Heartbeat(server.HeartbeatRequest{ID: "agent-alive", Host: "agent-alive.host"}); err != nil {
		t.Fatalf("Heartbeat(agent-alive): %v", err)
	}

	if got := reg.Get("agent-dead"); got != nil {
		t.Errorf("agent-dead still registered after 10×timeout of silence — stale entries must be pruned (got %+v)", got)
	}
	if got := reg.Get("agent-alive"); got == nil {
		t.Error("agent-alive pruned despite a fresh heartbeat — prune threshold is too aggressive")
	}
	all := reg.List(true)
	if len(all) != 1 || all[0].ID != "agent-alive" {
		t.Errorf("List(true) = %+v, want exactly [agent-alive] — no zombies in the fleet view", all)
	}

	// A pruned agent re-registers cleanly on its next heartbeat.
	if _, err := reg.Heartbeat(server.HeartbeatRequest{ID: "agent-dead", Host: "agent-dead.host"}); err != nil {
		t.Fatalf("re-registration Heartbeat: %v", err)
	}
	if got := reg.Get("agent-dead"); got == nil {
		t.Error("agent-dead did not re-register after pruning")
	}
}
