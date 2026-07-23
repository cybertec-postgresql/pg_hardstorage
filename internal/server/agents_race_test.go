// agents_race_test.go — regression tests for the AgentRegistry
// heartbeat slice race (concurrency audit, bug A).
//
// Before the fix, Heartbeat reused the Deployments backing array
// (append(a.Deployments[:0], ...)) while every snapshot (Heartbeat's
// returned copy, List, Get) copied the struct shallowly. Readers
// iterating a snapshot's Deployments after the registry lock was
// released raced with the next heartbeat rewriting the same backing
// array in place. TestAgentRegistry_HeartbeatSnapshotRace reproduces
// exactly that interleaving and fails under `go test -race` on the
// old code.

package server_test

import (
	"sync"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/server"
)

// TestAgentRegistry_HeartbeatSnapshotRace hammers Heartbeat for one
// agent with varying deployment lists while reader goroutines iterate
// the Deployments of List/Get snapshots outside the registry lock.
func TestAgentRegistry_HeartbeatSnapshotRace(t *testing.T) {
	reg := server.NewAgentRegistry(time.Minute)

	// Varying lengths, all <= the first list's length, so the old
	// append(a.Deployments[:0], ...) reused (and rewrote) the same
	// backing array on every heartbeat.
	lists := [][]string{
		{"deploy-alpha", "deploy-beta", "deploy-gamma"},
		{"deploy-delta", "deploy-epsilon"},
		{"deploy-zeta"},
		{"deploy-eta", "deploy-theta", "deploy-iota"},
	}

	const (
		iterations = 400
		readers    = 4
	)

	var wg sync.WaitGroup
	done := make(chan struct{})

	// Writer: re-heartbeat the same agent with rotating lists.
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(done)
		for i := 0; i < iterations; i++ {
			out, err := reg.Heartbeat(server.HeartbeatRequest{
				ID:          "agent-1",
				Host:        "host-1",
				Version:     "v1",
				Deployments: lists[i%len(lists)],
			})
			if err != nil {
				t.Errorf("Heartbeat: %v", err)
				return
			}
			// Read the returned snapshot outside the lock, like the
			// heartbeat HTTP handler does when it marshals it.
			consumeDeployments(out.Deployments)
		}
	}()

	// Readers: snapshot via List/Get, then iterate the strings after
	// the registry lock has been released (like handleAgents' JSON
	// marshal and refreshScrapeGauges).
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				for _, a := range reg.List(true) {
					consumeDeployments(a.Deployments)
				}
				if a := reg.Get("agent-1"); a != nil {
					consumeDeployments(a.Deployments)
				}
			}
		}()
	}

	wg.Wait()
}

// consumeDeployments reads every string element so the race detector
// observes the access to the slice's backing array.
func consumeDeployments(d []string) int {
	total := 0
	for _, s := range d {
		total += len(s)
	}
	return total
}

// TestAgentRegistry_SnapshotImmutable asserts the semantic contract:
// a snapshot taken from Heartbeat, Get, or List must not change when
// a later heartbeat replaces the agent's deployments.
func TestAgentRegistry_SnapshotImmutable(t *testing.T) {
	reg := server.NewAgentRegistry(time.Minute)

	first := []string{"deploy-a", "deploy-b"}
	hbSnap, err := reg.Heartbeat(server.HeartbeatRequest{
		ID: "agent-1", Host: "host-1", Deployments: first,
	})
	if err != nil {
		t.Fatal(err)
	}
	getSnap := reg.Get("agent-1")
	if getSnap == nil {
		t.Fatal("Get returned nil for a registered agent")
	}
	listSnap := reg.List(true)
	if len(listSnap) != 1 {
		t.Fatalf("List returned %d agents, want 1", len(listSnap))
	}

	// Overwrite with a different (shorter, then longer) list; the old
	// code rewrote the snapshots' shared backing array in place.
	for _, next := range [][]string{{"deploy-x"}, {"deploy-y", "deploy-z", "deploy-w"}} {
		if _, err := reg.Heartbeat(server.HeartbeatRequest{
			ID: "agent-1", Host: "host-1", Deployments: next,
		}); err != nil {
			t.Fatal(err)
		}
	}

	for name, got := range map[string][]string{
		"Heartbeat snapshot": hbSnap.Deployments,
		"Get snapshot":       getSnap.Deployments,
		"List snapshot":      listSnap[0].Deployments,
	} {
		if len(got) != len(first) {
			t.Fatalf("%s changed length after later heartbeats: got %v, want %v", name, got, first)
		}
		for i := range first {
			if got[i] != first[i] {
				t.Errorf("%s mutated by a later heartbeat: got %v, want %v", name, got, first)
				break
			}
		}
	}

	// The live record must reflect the latest heartbeat, of course.
	cur := reg.Get("agent-1")
	if cur == nil || len(cur.Deployments) != 3 || cur.Deployments[0] != "deploy-y" {
		t.Errorf("current record wrong after heartbeats: %+v", cur)
	}
}
