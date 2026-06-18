package follower

import (
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/patroni"
)

// makeMember builds a Member with the given role/state/lag.
// Pass lag=-1 for "lag not reported" (Patroni omits the field).
func makeMember(name string, role string, state string, lag int64) patroni.Member {
	m := patroni.Member{
		Name:  name,
		Role:  role,
		State: state,
		Host:  name + ".example",
		Port:  5432,
	}
	if lag >= 0 {
		m.Lag = &lag
	}
	return m
}

// TestPickReplica_NoMembers: empty input → no candidate.
func TestPickReplica_NoMembers(t *testing.T) {
	if _, ok := pickReplica(nil); ok {
		t.Error("pickReplica(nil) should return ok=false")
	}
}

// TestPickReplica_SkipsLeader: the leader is excluded by
// contract — pickReplica returns a non-leader or nothing.
func TestPickReplica_SkipsLeader(t *testing.T) {
	members := []patroni.Member{
		makeMember("node-1", "leader", "running", 0),
	}
	if _, ok := pickReplica(members); ok {
		t.Error("pickReplica should not pick the leader")
	}
}

// TestPickReplica_SkipsNonRunningStates: only state=running
// counts. Catches "stopped" / "starting" / "stopping" /
// "crashed" / unknown values.
func TestPickReplica_SkipsNonRunningStates(t *testing.T) {
	members := []patroni.Member{
		makeMember("node-1", "leader", "running", 0),
		makeMember("node-2", "replica", "stopped", 1024),
		makeMember("node-3", "replica", "starting", 0),
		makeMember("node-4", "replica", "crashed", 99999),
	}
	if _, ok := pickReplica(members); ok {
		t.Error("no running replicas → pickReplica should return ok=false")
	}
}

// TestPickReplica_PrefersLowestLag: of two running replicas,
// the one with lower lag wins. This is the headline behaviour:
// a near-caught-up replica makes a better dual-slot pin
// because its slot's restart_lsn tracks the leader more closely.
func TestPickReplica_PrefersLowestLag(t *testing.T) {
	members := []patroni.Member{
		makeMember("node-1", "leader", "running", 0),
		makeMember("laggy", "replica", "running", 10*1024*1024), // 10 MiB behind
		makeMember("fresh", "replica", "running", 4096),         // 4 KiB behind
		makeMember("middle", "replica", "running", 1024*1024),   // 1 MiB behind
	}
	picked, ok := pickReplica(members)
	if !ok {
		t.Fatal("expected a pick")
	}
	if picked.Name != "fresh" {
		t.Errorf("picked = %s (lag %d); want 'fresh' (lowest lag)", picked.Name, *picked.Lag)
	}
}

// TestPickReplica_PrefersWithLagOverWithout: a replica with
// reported lag wins over one whose lag is unreported. Patroni
// sometimes omits the field for stale members, so we treat
// "no lag reported" as a weak signal.
func TestPickReplica_PrefersWithLagOverWithout(t *testing.T) {
	members := []patroni.Member{
		makeMember("node-1", "leader", "running", 0),
		makeMember("stale", "replica", "running", -1),   // no lag reported
		makeMember("fresh", "replica", "running", 4096), // 4 KiB
	}
	picked, ok := pickReplica(members)
	if !ok {
		t.Fatal("expected a pick")
	}
	if picked.Name != "fresh" {
		t.Errorf("picked = %s; want 'fresh' (lag-reported beats lag-absent)", picked.Name)
	}
}

// TestPickReplica_FallsBackToWithoutLag: if the only running
// replicas have no lag reported, we still pick one (better
// than no replica at all).
func TestPickReplica_FallsBackToWithoutLag(t *testing.T) {
	members := []patroni.Member{
		makeMember("node-1", "leader", "running", 0),
		makeMember("stale", "replica", "running", -1),
	}
	picked, ok := pickReplica(members)
	if !ok {
		t.Fatal("expected a pick (fallback to without-lag)")
	}
	if picked.Name != "stale" {
		t.Errorf("picked = %s; want 'stale'", picked.Name)
	}
}

// TestPickReplica_StableOnTies: two replicas with identical
// lag values → the first one in /cluster order wins
// (deterministic). Pin so a future sort-instability change
// doesn't silently break operator expectations.
func TestPickReplica_StableOnTies(t *testing.T) {
	members := []patroni.Member{
		makeMember("node-1", "leader", "running", 0),
		makeMember("first", "replica", "running", 4096),
		makeMember("second", "replica", "running", 4096),
	}
	picked, _ := pickReplica(members)
	if picked.Name != "first" {
		t.Errorf("picked = %s; want 'first' (tie-break preserves member order)", picked.Name)
	}
}

// TestPickReplica_MasterAlias: Patroni sometimes reports the
// leader role as "master" (older versions). pickReplica
// uses Member.IsLeader() which accepts either.
func TestPickReplica_MasterAlias(t *testing.T) {
	members := []patroni.Member{
		makeMember("node-1", "master", "running", 0), // legacy role string
		makeMember("rep", "replica", "running", 4096),
	}
	picked, ok := pickReplica(members)
	if !ok {
		t.Fatal("expected a pick")
	}
	if picked.Name != "rep" {
		t.Errorf("picked = %s; want 'rep' (master should be excluded same as leader)", picked.Name)
	}
}

// TestPickReplica_SyncStandbyEligible: sync_standby is a
// special-purpose replica role; it's still a replica from
// pickReplica's perspective (only the leader is excluded).
// The caller (the agent's slot config) decides whether a
// sync_standby is the right pin; the picker just answers
// "is this a non-leader?".
func TestPickReplica_SyncStandbyEligible(t *testing.T) {
	members := []patroni.Member{
		makeMember("node-1", "leader", "running", 0),
		makeMember("syncrep", "sync_standby", "running", 0),
	}
	picked, ok := pickReplica(members)
	if !ok {
		t.Fatal("expected a pick")
	}
	if picked.Name != "syncrep" {
		t.Errorf("picked = %s; sync_standby should be eligible", picked.Name)
	}
}
