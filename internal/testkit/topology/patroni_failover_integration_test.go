// Build-tagged: brings up a real 3-node Spilo/Patroni cluster (1+ GB
// image, ~30 s start) and drives a real switchover, so it only runs
// under the integration tag / the Docker CI job.
//
//	go test -tags integration ./internal/testkit/topology/...
//
//go:build integration

package topology_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pglogrepl"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/replication"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/inject"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/topology"
)

// TestPatroniFailover_WALContinuityEndToEnd is the end-to-end failover
// test that was previously missing: it exercises pg_hardstorage's actual
// gap-detection / slot-recreation logic (`replication.EnsureSlot` — the
// core of the WAL "gap auditor" and the recreate-on-detection mechanism)
// against a REAL Patroni switchover, not a mocked /cluster endpoint or a
// simulated single-node DROP.
//
// It deliberately drives EnsureSlot over the topology's host-reachable
// ConnString (which re-discovers the leader on every call) rather than
// the follower.Coordinator: the Coordinator's DSNFor would have to reach
// Patroni-reported container-internal addresses, which a host-side test
// can't route. EnsureSlot is where the gap invariant lives; the
// Coordinator just plumbs the result (and is unit-tested separately).
//
// One cluster bring-up, several switchover phases (the cluster start is
// the expensive part; the switchovers are cheap).
func TestPatroniFailover_WALContinuityEndToEnd(t *testing.T) {
	topo, err := topology.Build("patroni-local-docker")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	if err := topo.Up(ctx, topology.UpOptions{}); err != nil {
		t.Fatalf("Up: %v", err)
	}
	defer func() {
		downCtx, c := context.WithTimeout(context.Background(), 90*time.Second)
		defer c()
		_ = topo.Down(downCtx)
	}()

	const slot = "hs_failover_e2e"

	// ---- Phase 1: create the agent's persistent slot on the leader,
	// capture its restart_lsn as the "last confirmed" position, and push
	// WAL forward so a lost-slot recreate has a non-trivial gap. ----
	leaderDSN := topo.ConnString()
	if leaderDSN == "" {
		t.Fatal("ConnString empty after Up")
	}
	lastConfirmed := func() pglogrepl.LSN {
		repl := mustConnect(t, ctx, leaderDSN, pg.ModeReplication)
		defer repl.Close(context.Background())
		reg := mustConnect(t, ctx, leaderDSN, pg.ModeRegular)
		defer reg.Close(context.Background())

		if err := replication.CreatePhysicalSlotReserveWAL(ctx, repl, slot); err != nil {
			t.Fatalf("create slot on leader: %v", err)
		}
		info, err := replication.GetSlot(ctx, reg, slot)
		if err != nil {
			t.Fatalf("read slot: %v", err)
		}
		if info.RestartLSN == "" {
			t.Fatal("restart_lsn empty after RESERVE_WAL")
		}
		lsn, err := pglogrepl.ParseLSN(info.RestartLSN)
		if err != nil {
			t.Fatalf("parse restart_lsn %q: %v", info.RestartLSN, err)
		}
		// Advance WAL well past last_confirmed so the post-failover gap
		// is unambiguous (3× a 16 MiB segment, each with real bytes).
		regExec(t, ctx, reg, "CREATE TABLE IF NOT EXISTS hs_failover_t (id int, v text)")
		for i := 0; i < 3; i++ {
			regExec(t, ctx, reg,
				fmt.Sprintf("INSERT INTO hs_failover_t SELECT g, repeat('x',1024) FROM generate_series(1,200) g OFFSET %d", i*1000))
			regExec(t, ctx, reg, "SELECT pg_switch_wal()")
		}
		regExec(t, ctx, reg, "CHECKPOINT")
		t.Logf("phase 1: slot %q restart_lsn=%s on initial leader", slot, lsn)
		return lsn
	}()

	// ---- Phase 2: a real Patroni switchover. The custom slot is not a
	// Patroni permanent_slot, so it does NOT travel to the new leader. ----
	newDSN := switchover(t, ctx, topo, leaderDSN)

	// ---- Phase 3: on the NEW leader the slot is gone; EnsureSlot must
	// recreate it AND report the gap relative to last_confirmed. This is
	// the gap auditor's whole reason to exist. ----
	newRestart := func() pglogrepl.LSN {
		repl := mustConnect(t, ctx, newDSN, pg.ModeReplication)
		defer repl.Close(context.Background())
		reg := mustConnect(t, ctx, newDSN, pg.ModeRegular)
		defer reg.Close(context.Background())

		res, err := replication.EnsureSlot(ctx, reg, repl, slot, lastConfirmed)
		if err != nil {
			t.Fatalf("EnsureSlot on new leader: %v", err)
		}
		if res.Outcome != replication.SlotRecreated {
			t.Errorf("Outcome = %q, want %q (a non-permanent slot must be gone after switchover)",
				res.Outcome, replication.SlotRecreated)
		}
		if !res.HasGap() {
			t.Errorf("HasGap() = false; a switchover that lost the slot after %d bytes of WAL must report a gap", res.GapBytes)
		}
		if res.GapStartLSN != lastConfirmed {
			t.Errorf("GapStartLSN = %s, want %s (last_confirmed)", res.GapStartLSN, lastConfirmed)
		}
		if res.GapEndLSN <= lastConfirmed {
			t.Errorf("GapEndLSN = %s, want > %s (new leader has advanced)", res.GapEndLSN, lastConfirmed)
		}
		if want := uint64(res.GapEndLSN - res.GapStartLSN); res.GapBytes != want {
			t.Errorf("GapBytes = %d, want %d (end-start)", res.GapBytes, want)
		}
		t.Logf("phase 3: gap after switchover start=%s end=%s bytes=%d (~%d MiB)",
			res.GapStartLSN, res.GapEndLSN, res.GapBytes, res.GapBytes>>20)
		return res.GapEndLSN
	}()

	// ---- Phase 4: steady state. The slot now exists on the new leader;
	// re-running EnsureSlot with the up-to-date confirmed position must
	// find it and report no gap (no false alarms once recovered). ----
	func() {
		repl := mustConnect(t, ctx, newDSN, pg.ModeReplication)
		defer repl.Close(context.Background())
		reg := mustConnect(t, ctx, newDSN, pg.ModeRegular)
		defer reg.Close(context.Background())

		res, err := replication.EnsureSlot(ctx, reg, repl, slot, newRestart)
		if err != nil {
			t.Fatalf("EnsureSlot steady-state: %v", err)
		}
		if res.Outcome != replication.SlotFound {
			t.Errorf("Outcome = %q, want %q (slot is present after recreate)", res.Outcome, replication.SlotFound)
		}
		if res.HasGap() {
			t.Errorf("HasGap() = true on a present, up-to-date slot — false alarm (GapBytes=%d)", res.GapBytes)
		}
		t.Logf("phase 4: steady-state EnsureSlot found the slot with no gap")
	}()

	// ---- Phase 5: a second switchover (failback), then WAL keeps flowing
	// on the NEW leader before the agent reconciles. The recreate+gap path
	// must hold across repeated leader changes.
	//
	// Subtlety this guards against (a flake an earlier version hit):
	// whether a gap EXISTS is a property of WAL actually moving past
	// last_confirmed, not of the switchover itself. last_confirmed here is
	// the phase-3 recreate position, which sits right at the cluster's
	// current WAL position — so advancing WAL on the OLD leader (where our
	// RESERVE_WAL slot pins restart_lsn) could leave the recreated slot on
	// the new leader landing back at last_confirmed, i.e. gap 0 (the
	// product is correct — nothing was lost). To assert a gap
	// deterministically we drive WAL forward ON THE PROMOTED LEADER and
	// CHECKPOINT before reconciling, so the recreate's RESERVE_WAL
	// restart_lsn lands at the current redo, strictly past last_confirmed. ----
	new2DSN := switchover(t, ctx, topo, newDSN)
	func() {
		repl := mustConnect(t, ctx, new2DSN, pg.ModeReplication)
		defer repl.Close(context.Background())
		reg := mustConnect(t, ctx, new2DSN, pg.ModeRegular)
		defer reg.Close(context.Background())
		// Best-effort cleanup of our slot on whichever node ends up leader.
		t.Cleanup(func() {
			dctx, dc := context.WithTimeout(context.Background(), 10*time.Second)
			defer dc()
			if rc, err := pg.Connect(dctx, topo.ConnString(), pg.ModeReplication); err == nil {
				_ = replication.DropSlot(dctx, rc, slot)
				_ = rc.Close(context.Background())
			}
		})

		// WAL flows on the promoted leader before the agent reconnects.
		for i := 0; i < 3; i++ {
			regExec(t, ctx, reg,
				fmt.Sprintf("INSERT INTO hs_failover_t SELECT g, repeat('y',1024) FROM generate_series(1,200) g OFFSET %d", i*1000))
			regExec(t, ctx, reg, "SELECT pg_switch_wal()")
		}
		regExec(t, ctx, reg, "CHECKPOINT")

		res, err := replication.EnsureSlot(ctx, reg, repl, slot, newRestart)
		if err != nil {
			t.Fatalf("EnsureSlot after second switchover: %v", err)
		}
		if res.Outcome != replication.SlotRecreated {
			t.Errorf("second switchover: Outcome = %q, want %q", res.Outcome, replication.SlotRecreated)
		}
		if !res.HasGap() {
			t.Errorf("second switchover: expected a gap after advancing WAL on the promoted leader (last_confirmed=%s, GapBytes=%d)", newRestart, res.GapBytes)
		}
		if res.GapStartLSN != newRestart {
			t.Errorf("second switchover: GapStartLSN = %s, want %s", res.GapStartLSN, newRestart)
		}
		if res.GapEndLSN <= newRestart {
			t.Errorf("second switchover: GapEndLSN = %s, want > %s", res.GapEndLSN, newRestart)
		}
		t.Logf("phase 5: second switchover again lost the slot and reported a gap (bytes=%d, ~%d MiB)", res.GapBytes, res.GapBytes>>20)
	}()
}

// TestPatroniFailover_MultiSlotAndBootstrap covers two more failover
// behaviours against a real cluster:
//
//   - Multi-slot reconcile (the README's Mechanism 3): several persistent
//     slots are each independently recreated, with their own gap, after a
//     switchover.
//   - First-time bootstrap: a brand-new slot created on the post-failover
//     leader with no prior confirmed position must report NO gap — a fresh
//     agent must not false-alarm just because it happened to connect right
//     after a leader change.
func TestPatroniFailover_MultiSlotAndBootstrap(t *testing.T) {
	topo, err := topology.Build("patroni-local-docker")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	if err := topo.Up(ctx, topology.UpOptions{}); err != nil {
		t.Fatalf("Up: %v", err)
	}
	defer func() {
		dctx, c := context.WithTimeout(context.Background(), 90*time.Second)
		defer c()
		_ = topo.Down(dctx)
	}()

	leaderDSN := topo.ConnString()
	if leaderDSN == "" {
		t.Fatal("ConnString empty after Up")
	}
	slots := []string{"hs_ms_a", "hs_ms_b"}
	confirmed := map[string]pglogrepl.LSN{}

	// Create both slots on the leader, capture each restart_lsn, advance WAL.
	func() {
		repl := mustConnect(t, ctx, leaderDSN, pg.ModeReplication)
		defer repl.Close(context.Background())
		reg := mustConnect(t, ctx, leaderDSN, pg.ModeRegular)
		defer reg.Close(context.Background())
		for _, s := range slots {
			if err := replication.CreatePhysicalSlotReserveWAL(ctx, repl, s); err != nil {
				t.Fatalf("create slot %s: %v", s, err)
			}
			info, err := replication.GetSlot(ctx, reg, s)
			if err != nil {
				t.Fatalf("get slot %s: %v", s, err)
			}
			lsn, err := pglogrepl.ParseLSN(info.RestartLSN)
			if err != nil {
				t.Fatalf("parse restart_lsn for %s (%q): %v", s, info.RestartLSN, err)
			}
			confirmed[s] = lsn
		}
		regExec(t, ctx, reg, "CREATE TABLE IF NOT EXISTS hs_ms_t (id int, v text)")
		for i := 0; i < 3; i++ {
			regExec(t, ctx, reg,
				fmt.Sprintf("INSERT INTO hs_ms_t SELECT g, repeat('z',1024) FROM generate_series(1,200) g OFFSET %d", i*1000))
			regExec(t, ctx, reg, "SELECT pg_switch_wal()")
		}
		regExec(t, ctx, reg, "CHECKPOINT")
	}()

	newDSN := switchover(t, ctx, topo, leaderDSN)

	func() {
		repl := mustConnect(t, ctx, newDSN, pg.ModeReplication)
		defer repl.Close(context.Background())
		reg := mustConnect(t, ctx, newDSN, pg.ModeRegular)
		defer reg.Close(context.Background())
		t.Cleanup(func() {
			dctx, dc := context.WithTimeout(context.Background(), 15*time.Second)
			defer dc()
			if rc, err := pg.Connect(dctx, topo.ConnString(), pg.ModeReplication); err == nil {
				for _, s := range append(append([]string{}, slots...), "hs_bootstrap") {
					_ = replication.DropSlot(dctx, rc, s)
				}
				_ = rc.Close(context.Background())
			}
		})

		// Multi-slot: each lost slot is recreated, each with its own gap.
		for _, s := range slots {
			res, err := replication.EnsureSlot(ctx, reg, repl, s, confirmed[s])
			if err != nil {
				t.Fatalf("EnsureSlot %s on new leader: %v", s, err)
			}
			if res.Outcome != replication.SlotRecreated {
				t.Errorf("%s: Outcome = %q, want %q", s, res.Outcome, replication.SlotRecreated)
			}
			if !res.HasGap() {
				t.Errorf("%s: expected a gap after switchover (GapBytes=%d)", s, res.GapBytes)
			}
			if res.GapStartLSN != confirmed[s] {
				t.Errorf("%s: GapStartLSN = %s, want %s", s, res.GapStartLSN, confirmed[s])
			}
			t.Logf("multi-slot: %s recreated with gap bytes=%d", s, res.GapBytes)
		}

		// Bootstrap: a brand-new slot with no prior position must NOT
		// report a gap, even though we're on a just-promoted leader.
		res, err := replication.EnsureSlot(ctx, reg, repl, "hs_bootstrap", pglogrepl.LSN(0))
		if err != nil {
			t.Fatalf("EnsureSlot bootstrap: %v", err)
		}
		if res.Outcome != replication.SlotRecreated {
			t.Errorf("bootstrap: Outcome = %q, want %q", res.Outcome, replication.SlotRecreated)
		}
		if res.HasGap() {
			t.Errorf("bootstrap: false-alarm gap on a first-ever slot (GapBytes=%d)", res.GapBytes)
		}
		t.Logf("bootstrap: fresh slot on the post-failover leader correctly reported no gap")
	}()
}

// TestPatroniFailover_PermanentSlotSurvivesSwitchover proves the README's
// Mechanism 1 (permanent slots): a slot declared in Patroni's
// `permanent_slots` is carried across a switchover by Patroni itself, so
// the agent FINDS it on the new leader (Outcome=SlotFound) rather than
// recreating it — the gap-PREVENTION counterpart to the gap-detection
// covered by TestPatroniFailover_WALContinuityEndToEnd.
func TestPatroniFailover_PermanentSlotSurvivesSwitchover(t *testing.T) {
	topo, err := topology.Build("patroni-local-docker")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 9*time.Minute)
	defer cancel()
	if err := topo.Up(ctx, topology.UpOptions{}); err != nil {
		t.Fatalf("Up: %v", err)
	}
	defer func() {
		dctx, c := context.WithTimeout(context.Background(), 90*time.Second)
		defer c()
		_ = topo.Down(dctx)
	}()

	const slot = "hs_perm"

	// Declare the slot in Patroni's cluster-wide config as a permanent
	// physical slot. Any member can edit the DCS config; Patroni then
	// maintains the slot on the leader and re-establishes it on a new
	// leader after a switchover.
	pc := firstPatroniContainer(t, topo)
	dockerExec(t, ctx, pc, "patronictl", "edit-config", "--force", "-s", "slots."+slot+".type=physical")

	// Wait for Patroni to materialise the permanent slot on the leader.
	lastConfirmed := waitForSlotLSN(t, ctx, topo, slot, 90*time.Second)
	leaderDSN := topo.ConnString()
	t.Logf("permanent slot %q present on initial leader at restart_lsn=%s", slot, lastConfirmed)

	newDSN := switchover(t, ctx, topo, leaderDSN)

	// Patroni re-establishes the permanent slot on the promoted leader.
	// Give it a moment to appear, then assert the agent finds it rather
	// than recreating it — that's Mechanism 1 working.
	_ = waitForSlotLSN(t, ctx, topo, slot, 90*time.Second)

	repl := mustConnect(t, ctx, newDSN, pg.ModeReplication)
	defer repl.Close(context.Background())
	reg := mustConnect(t, ctx, newDSN, pg.ModeRegular)
	defer reg.Close(context.Background())

	res, err := replication.EnsureSlot(ctx, reg, repl, slot, lastConfirmed)
	if err != nil {
		t.Fatalf("EnsureSlot on new leader: %v", err)
	}
	if res.Outcome != replication.SlotFound {
		t.Errorf("permanent slot must survive the switchover (Mechanism 1); Outcome = %q, want %q",
			res.Outcome, replication.SlotFound)
	}
	t.Logf("permanent slot survived the switchover: EnsureSlot outcome=%s, gap=%v", res.Outcome, res.HasGap())
}

// firstPatroniContainer returns the container name of any patroni-role
// node so the test can `docker exec patronictl …` against the cluster.
func firstPatroniContainer(t *testing.T, topo topology.Topology) string {
	t.Helper()
	for _, tg := range topo.Targets() {
		if dt, ok := tg.(*inject.DockerTarget); ok && dt.RoleStr == "patroni" {
			return dt.Container
		}
	}
	t.Fatal("no patroni-role docker target found")
	return ""
}

// dockerExec runs `docker exec <container> <args…>`, failing the test on
// a non-zero exit with the combined output for diagnosis.
func dockerExec(t *testing.T, ctx context.Context, container string, args ...string) {
	t.Helper()
	out, err := exec.CommandContext(ctx, "docker", append([]string{"exec", container}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker exec %s %v: %v\n%s", container, args, err, out)
	}
}

// waitForSlotLSN polls the current leader until the named slot exists with
// a populated restart_lsn, returning it. Fails the test on timeout.
func waitForSlotLSN(t *testing.T, ctx context.Context, topo topology.Topology, slot string, timeout time.Duration) pglogrepl.LSN {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if dsn := topo.ConnString(); dsn != "" {
			if reg, err := pg.Connect(ctx, dsn, pg.ModeRegular); err == nil {
				info, gerr := replication.GetSlot(ctx, reg, slot)
				_ = reg.Close(context.Background())
				if gerr == nil && info.RestartLSN != "" {
					if lsn, perr := pglogrepl.ParseLSN(info.RestartLSN); perr == nil {
						return lsn
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("ctx cancelled waiting for slot %q", slot)
		case <-time.After(2 * time.Second):
		}
	}
	t.Fatalf("slot %q did not appear within %s", slot, timeout)
	return 0
}

// switchover fires a real patroni_switchover and blocks until the
// topology resolves a NEW, writable primary (different DSN, not in
// recovery). Returns the new leader DSN. Mirrors the poll in
// TestPatroniLocalDocker_LifecycleAndFailover.
func switchover(t *testing.T, ctx context.Context, topo topology.Topology, prevDSN string) string {
	t.Helper()
	ts := inject.NewStaticTargetSet(topo.Targets(), time.Now().UnixNano())
	if _, err := inject.DefaultRegistry.Apply(ctx, "patroni_switchover(target=patroni)", ts); err != nil {
		t.Fatalf("apply patroni_switchover: %v", err)
	}
	dsn := waitForNewWritableLeader(t, ctx, topo, prevDSN, 90*time.Second)
	t.Logf("switchover complete: new leader %s", dsn)
	return dsn
}

// waitForNewWritableLeader blocks until the topology resolves a NEW,
// writable primary (DSN != prevDSN and pg_is_in_recovery() is false) and
// returns it. Shared by the graceful-switchover and hard-kill paths.
func waitForNewWritableLeader(t *testing.T, ctx context.Context, topo topology.Topology, prevDSN string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		cur := topo.ConnString()
		if cur != "" && cur != prevDSN {
			db, err := sql.Open("pgx", cur)
			if err == nil {
				var inRecovery bool
				qctx, qc := context.WithTimeout(ctx, 5*time.Second)
				err = db.QueryRowContext(qctx, "SELECT pg_is_in_recovery()").Scan(&inRecovery)
				qc()
				_ = db.Close()
				if err == nil && !inRecovery {
					return cur
				}
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("ctx cancelled waiting for new leader (prev=%s, cur=%s)", prevDSN, topo.ConnString())
		case <-time.After(2 * time.Second):
		}
	}
	t.Fatalf("no new writable primary within %s (prev=%s, cur=%s)", timeout, prevDSN, topo.ConnString())
	return ""
}

func mustConnect(t *testing.T, ctx context.Context, dsn string, mode pg.Mode) *pg.Conn {
	t.Helper()
	c, err := pg.Connect(ctx, dsn, mode)
	if err != nil {
		t.Fatalf("pg.Connect (%s) to %s: %v", mode, dsn, err)
	}
	return c
}

func regExec(t *testing.T, ctx context.Context, c *pg.Conn, sql string) {
	t.Helper()
	r := c.PgConn().ExecParams(ctx, sql, nil, nil, nil, nil)
	if _, err := r.Close(); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// TestPatroniFailover_HardLeaderKill is the unplanned-failover counterpart
// to the graceful switchover tests: it SIGKILLs the leader's container —
// no handoff — and asserts that, once Patroni's lease expires and a
// replica is promoted, the agent recreates its (non-permanent) slot on the
// new leader and reports the gap. Real outages rarely arrive as a polite
// switchover, so this is the path that matters most.
func TestPatroniFailover_HardLeaderKill(t *testing.T) {
	topo, err := topology.Build("patroni-local-docker")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 9*time.Minute)
	defer cancel()
	if err := topo.Up(ctx, topology.UpOptions{}); err != nil {
		t.Fatalf("Up: %v", err)
	}
	defer func() {
		dctx, c := context.WithTimeout(context.Background(), 90*time.Second)
		defer c()
		_ = topo.Down(dctx)
	}()

	leaderDSN := topo.ConnString()
	if leaderDSN == "" {
		t.Fatal("ConnString empty after Up")
	}
	const slot = "hs_hardkill"

	lastConfirmed := func() pglogrepl.LSN {
		repl := mustConnect(t, ctx, leaderDSN, pg.ModeReplication)
		defer repl.Close(context.Background())
		reg := mustConnect(t, ctx, leaderDSN, pg.ModeRegular)
		defer reg.Close(context.Background())
		if err := replication.CreatePhysicalSlotReserveWAL(ctx, repl, slot); err != nil {
			t.Fatalf("create slot: %v", err)
		}
		info, err := replication.GetSlot(ctx, reg, slot)
		if err != nil {
			t.Fatalf("get slot: %v", err)
		}
		lsn, err := pglogrepl.ParseLSN(info.RestartLSN)
		if err != nil {
			t.Fatalf("parse restart_lsn %q: %v", info.RestartLSN, err)
		}
		regExec(t, ctx, reg, "CREATE TABLE IF NOT EXISTS hs_hk_t (id int, v text)")
		for i := 0; i < 3; i++ {
			regExec(t, ctx, reg,
				fmt.Sprintf("INSERT INTO hs_hk_t SELECT g, repeat('k',1024) FROM generate_series(1,200) g OFFSET %d", i*1000))
			regExec(t, ctx, reg, "SELECT pg_switch_wal()")
		}
		regExec(t, ctx, reg, "CHECKPOINT")
		t.Logf("hard kill: slot %q restart_lsn=%s on initial leader", slot, lsn)
		return lsn
	}()

	// The hard part: SIGKILL the leader container. No graceful demotion.
	killLeaderContainer(t, ctx, topo, leaderDSN)

	// Patroni promotes a replica once the dead leader's lease expires;
	// allow a generous window for the TTL + election + promotion.
	newDSN := waitForNewWritableLeader(t, ctx, topo, leaderDSN, 150*time.Second)
	t.Logf("hard kill: new leader elected at %s", newDSN)

	repl := mustConnect(t, ctx, newDSN, pg.ModeReplication)
	defer repl.Close(context.Background())
	reg := mustConnect(t, ctx, newDSN, pg.ModeRegular)
	defer reg.Close(context.Background())
	t.Cleanup(func() {
		dctx, dc := context.WithTimeout(context.Background(), 10*time.Second)
		defer dc()
		if rc, err := pg.Connect(dctx, topo.ConnString(), pg.ModeReplication); err == nil {
			_ = replication.DropSlot(dctx, rc, slot)
			_ = rc.Close(context.Background())
		}
	})

	res, err := replication.EnsureSlot(ctx, reg, repl, slot, lastConfirmed)
	if err != nil {
		t.Fatalf("EnsureSlot after hard kill: %v", err)
	}
	if res.Outcome != replication.SlotRecreated {
		t.Errorf("hard kill: Outcome = %q, want %q (slot lived on the killed leader)", res.Outcome, replication.SlotRecreated)
	}
	if !res.HasGap() {
		t.Errorf("hard kill: expected a gap after promotion (GapBytes=%d)", res.GapBytes)
	}
	if res.GapStartLSN != lastConfirmed {
		t.Errorf("hard kill: GapStartLSN = %s, want %s", res.GapStartLSN, lastConfirmed)
	}
	if res.GapEndLSN <= lastConfirmed {
		t.Errorf("hard kill: GapEndLSN = %s, want > %s", res.GapEndLSN, lastConfirmed)
	}
	t.Logf("hard kill: slot recreated on the promoted leader with gap bytes=%d (~%d MiB)", res.GapBytes, res.GapBytes>>20)
}

// killLeaderContainer SIGKILLs whichever cluster node is currently the
// leader, found by matching the leader DSN's published port to a
// container's `docker port 5432` mapping. Simulates an unplanned failover
// (no graceful Patroni handoff).
func killLeaderContainer(t *testing.T, ctx context.Context, topo topology.Topology, leaderDSN string) {
	t.Helper()
	u, err := url.Parse(leaderDSN)
	if err != nil || u.Port() == "" {
		t.Fatalf("parse leader DSN %q: %v", leaderDSN, err)
	}
	leaderPort := u.Port()
	for _, tg := range topo.Targets() {
		dt, ok := tg.(*inject.DockerTarget)
		if !ok || dt.RoleStr != "patroni" {
			continue
		}
		out, perr := exec.CommandContext(ctx, "docker", "port", dt.Container, "5432/tcp").CombinedOutput()
		if perr != nil {
			continue
		}
		if port := hostPortOf(string(out)); port == leaderPort {
			t.Logf("hard-killing leader container %s (pg port %s)", dt.Container, leaderPort)
			if kout, kerr := exec.CommandContext(ctx, "docker", "kill", dt.Container).CombinedOutput(); kerr != nil {
				t.Fatalf("docker kill %s: %v\n%s", dt.Container, kerr, kout)
			}
			return
		}
	}
	t.Fatalf("could not find the leader container for pg port %s", leaderPort)
}

// hostPortOf extracts the host port from `docker port` output such as
// "127.0.0.1:45981" (taking the first line if several are present).
func hostPortOf(dockerPortOut string) string {
	line := strings.TrimSpace(dockerPortOut)
	if nl := strings.IndexByte(line, '\n'); nl >= 0 {
		line = strings.TrimSpace(line[:nl])
	}
	if i := strings.LastIndex(line, ":"); i >= 0 && i < len(line)-1 {
		return strings.TrimSpace(line[i+1:])
	}
	return ""
}

// containerForPort returns the patroni container whose published 5432 maps
// to the given host port.
func containerForPort(ctx context.Context, topo topology.Topology, port string) (string, bool) {
	for _, tg := range topo.Targets() {
		dt, ok := tg.(*inject.DockerTarget)
		if !ok || dt.RoleStr != "patroni" {
			continue
		}
		out, err := exec.CommandContext(ctx, "docker", "port", dt.Container, "5432/tcp").CombinedOutput()
		if err != nil {
			continue
		}
		if hostPortOf(string(out)) == port {
			return dt.Container, true
		}
	}
	return "", false
}

// leaderContainer returns the container currently backing the leader DSN.
func leaderContainer(t *testing.T, ctx context.Context, topo topology.Topology, leaderDSN string) string {
	t.Helper()
	u, err := url.Parse(leaderDSN)
	if err != nil || u.Port() == "" {
		t.Fatalf("parse leader DSN %q: %v", leaderDSN, err)
	}
	c, ok := containerForPort(ctx, topo, u.Port())
	if !ok {
		t.Fatalf("could not find leader container for pg port %s", u.Port())
	}
	return c
}

// aReplicaContainer returns any patroni container that is NOT the current
// leader (a replica).
func aReplicaContainer(t *testing.T, ctx context.Context, topo topology.Topology, leaderDSN string) string {
	t.Helper()
	u, err := url.Parse(leaderDSN)
	if err != nil || u.Port() == "" {
		t.Fatalf("parse leader DSN %q: %v", leaderDSN, err)
	}
	leaderPort := u.Port()
	for _, tg := range topo.Targets() {
		dt, ok := tg.(*inject.DockerTarget)
		if !ok || dt.RoleStr != "patroni" {
			continue
		}
		out, err := exec.CommandContext(ctx, "docker", "port", dt.Container, "5432/tcp").CombinedOutput()
		if err != nil {
			continue
		}
		if p := hostPortOf(string(out)); p != "" && p != leaderPort {
			return dt.Container
		}
	}
	t.Fatalf("no replica container found (leader port %s)", leaderPort)
	return ""
}

// dockerMust runs `docker <args…>`, failing the test on a non-zero exit.
func dockerMust(t *testing.T, ctx context.Context, args ...string) {
	t.Helper()
	if out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput(); err != nil {
		t.Fatalf("docker %v: %v\n%s", args, err, out)
	}
}

// patroniMember is the subset of `patronictl list -f json` we assert on.
type patroniMember struct {
	Member string `json:"Member"`
	Role   string `json:"Role"`
	State  string `json:"State"`
}

// clusterMembersAny runs `patronictl list -f json` against whichever
// patroni container answers first (any healthy node can report the DCS
// view), returning the parsed membership.
func clusterMembersAny(ctx context.Context, topo topology.Topology) ([]patroniMember, bool) {
	for _, tg := range topo.Targets() {
		dt, ok := tg.(*inject.DockerTarget)
		if !ok || dt.RoleStr != "patroni" {
			continue
		}
		out, err := exec.CommandContext(ctx, "docker", "exec", dt.Container, "patronictl", "list", "-f", "json").Output()
		if err != nil {
			continue
		}
		var ms []patroniMember
		if json.Unmarshal(out, &ms) == nil && len(ms) > 0 {
			return ms, true
		}
	}
	return nil, false
}

// waitForHealthyCluster blocks until the cluster reports exactly wantTotal
// members with exactly one Leader and every member in a running/streaming
// state — i.e. a fully recovered cluster. Fails on timeout.
func waitForHealthyCluster(t *testing.T, ctx context.Context, topo topology.Topology, wantTotal int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last []patroniMember
	for time.Now().Before(deadline) {
		if ms, ok := clusterMembersAny(ctx, topo); ok {
			last = ms
			leaders, healthy := 0, 0
			for _, m := range ms {
				if m.Role == "Leader" {
					leaders++
				}
				if m.State == "running" || m.State == "streaming" {
					healthy++
				}
			}
			if len(ms) == wantTotal && leaders == 1 && healthy == wantTotal {
				t.Logf("cluster healthy: %d members, 1 leader, all running/streaming", wantTotal)
				return
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("ctx cancelled waiting for healthy cluster (last=%v)", last)
		case <-time.After(3 * time.Second):
		}
	}
	t.Fatalf("cluster did not reach %d healthy members with one leader within %s (last=%v)", wantTotal, timeout, last)
}

// TestPatroniRecovery_KilledLeaderRejoinsAsReplica covers the full failure
// AND recovery cycle: SIGKILL the leader, let a replica be promoted (and the
// agent recreate its slot with a gap), then bring the dead node back with
// `docker start` and assert it rejoins the cluster as a healthy replica —
// 3 members, one leader, all streaming.
func TestPatroniRecovery_KilledLeaderRejoinsAsReplica(t *testing.T) {
	topo, err := topology.Build("patroni-local-docker")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	if err := topo.Up(ctx, topology.UpOptions{}); err != nil {
		t.Fatalf("Up: %v", err)
	}
	defer func() {
		dctx, c := context.WithTimeout(context.Background(), 90*time.Second)
		defer c()
		_ = topo.Down(dctx)
	}()

	leaderDSN := topo.ConnString()
	if leaderDSN == "" {
		t.Fatal("ConnString empty after Up")
	}
	const slot = "hs_rejoin"

	lastConfirmed := func() pglogrepl.LSN {
		repl := mustConnect(t, ctx, leaderDSN, pg.ModeReplication)
		defer repl.Close(context.Background())
		reg := mustConnect(t, ctx, leaderDSN, pg.ModeRegular)
		defer reg.Close(context.Background())
		if err := replication.CreatePhysicalSlotReserveWAL(ctx, repl, slot); err != nil {
			t.Fatalf("create slot: %v", err)
		}
		info, err := replication.GetSlot(ctx, reg, slot)
		if err != nil {
			t.Fatalf("get slot: %v", err)
		}
		lsn, _ := pglogrepl.ParseLSN(info.RestartLSN)
		regExec(t, ctx, reg, "CREATE TABLE IF NOT EXISTS hs_rejoin_t (id int, v text)")
		for i := 0; i < 3; i++ {
			regExec(t, ctx, reg,
				fmt.Sprintf("INSERT INTO hs_rejoin_t SELECT g, repeat('r',1024) FROM generate_series(1,200) g OFFSET %d", i*1000))
			regExec(t, ctx, reg, "SELECT pg_switch_wal()")
		}
		regExec(t, ctx, reg, "CHECKPOINT")
		return lsn
	}()

	// Kill the leader, capturing its container so we can revive it later.
	dead := leaderContainer(t, ctx, topo, leaderDSN)
	t.Logf("killing leader container %s", dead)
	dockerMust(t, ctx, "kill", dead)

	newDSN := waitForNewWritableLeader(t, ctx, topo, leaderDSN, 150*time.Second)
	t.Logf("new leader elected at %s", newDSN)

	// Agent reconciles on the new leader: slot lost → recreated → gap.
	func() {
		repl := mustConnect(t, ctx, newDSN, pg.ModeReplication)
		defer repl.Close(context.Background())
		reg := mustConnect(t, ctx, newDSN, pg.ModeRegular)
		defer reg.Close(context.Background())
		t.Cleanup(func() {
			dctx, dc := context.WithTimeout(context.Background(), 10*time.Second)
			defer dc()
			if rc, err := pg.Connect(dctx, topo.ConnString(), pg.ModeReplication); err == nil {
				_ = replication.DropSlot(dctx, rc, slot)
				_ = rc.Close(context.Background())
			}
		})
		res, err := replication.EnsureSlot(ctx, reg, repl, slot, lastConfirmed)
		if err != nil {
			t.Fatalf("EnsureSlot on new leader: %v", err)
		}
		if res.Outcome != replication.SlotRecreated || !res.HasGap() {
			t.Errorf("expected SlotRecreated with a gap after kill; got %s gap=%v", res.Outcome, res.HasGap())
		}
		t.Logf("slot recreated on new leader, gap bytes=%d", res.GapBytes)
	}()

	// Recovery: revive the dead node; it must rejoin as a healthy replica.
	t.Logf("reviving killed node %s", dead)
	dockerMust(t, ctx, "start", dead)
	waitForHealthyCluster(t, ctx, topo, 3, 4*time.Minute)
}

// TestPatroniRecovery_ReplicaLossKeepsPrimary asserts that losing a REPLICA
// is a non-event for the primary: no failover, the leader stays writable and
// keeps the agent's slot, and once the replica is revived the cluster
// returns to full health.
func TestPatroniRecovery_ReplicaLossKeepsPrimary(t *testing.T) {
	topo, err := topology.Build("patroni-local-docker")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	if err := topo.Up(ctx, topology.UpOptions{}); err != nil {
		t.Fatalf("Up: %v", err)
	}
	defer func() {
		dctx, c := context.WithTimeout(context.Background(), 90*time.Second)
		defer c()
		_ = topo.Down(dctx)
	}()

	leaderDSN := topo.ConnString()
	if leaderDSN == "" {
		t.Fatal("ConnString empty after Up")
	}
	const slot = "hs_replica_loss"
	func() {
		repl := mustConnect(t, ctx, leaderDSN, pg.ModeReplication)
		defer repl.Close(context.Background())
		if err := replication.CreatePhysicalSlotReserveWAL(ctx, repl, slot); err != nil {
			t.Fatalf("create slot: %v", err)
		}
	}()
	t.Cleanup(func() {
		dctx, dc := context.WithTimeout(context.Background(), 10*time.Second)
		defer dc()
		if rc, err := pg.Connect(dctx, topo.ConnString(), pg.ModeReplication); err == nil {
			_ = replication.DropSlot(dctx, rc, slot)
			_ = rc.Close(context.Background())
		}
	})

	// Kill a replica — NOT the leader.
	rep := aReplicaContainer(t, ctx, topo, leaderDSN)
	t.Logf("killing replica container %s", rep)
	dockerMust(t, ctx, "kill", rep)

	// For a stretch of time the leader must remain the SAME node, stay
	// writable, and keep the slot. No spurious failover on replica loss.
	stableUntil := time.Now().Add(20 * time.Second)
	for time.Now().Before(stableUntil) {
		if cur := topo.ConnString(); cur != leaderDSN {
			t.Fatalf("leader changed after a replica was killed (was %s, now %s) — unexpected failover", leaderDSN, cur)
		}
		select {
		case <-ctx.Done():
			t.Fatal("ctx cancelled during stability window")
		case <-time.After(3 * time.Second):
		}
	}
	func() {
		reg := mustConnect(t, ctx, leaderDSN, pg.ModeRegular)
		defer reg.Close(context.Background())
		// Leader still reachable, and the agent's slot survived intact.
		regExec(t, ctx, reg, "SELECT 1")
		if _, err := replication.GetSlot(ctx, reg, slot); err != nil {
			t.Errorf("agent slot missing on the still-healthy leader after replica loss: %v", err)
		}
	}()
	t.Logf("primary unaffected by replica loss; slot intact")

	// Recovery: revive the replica; cluster must return to full health.
	t.Logf("reviving replica %s", rep)
	dockerMust(t, ctx, "start", rep)
	waitForHealthyCluster(t, ctx, topo, 3, 4*time.Minute)
}

// TestPatroniRecovery_FrozenLeaderFailoverAndRejoin simulates a stalled
// node (VM freeze / long GC / network blackhole) via `docker pause`: the
// leader stops renewing its lease, a replica is promoted, then the leader is
// unpaused and must demote and rejoin as a healthy replica.
func TestPatroniRecovery_FrozenLeaderFailoverAndRejoin(t *testing.T) {
	topo, err := topology.Build("patroni-local-docker")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	if err := topo.Up(ctx, topology.UpOptions{}); err != nil {
		t.Fatalf("Up: %v", err)
	}
	defer func() {
		dctx, c := context.WithTimeout(context.Background(), 90*time.Second)
		defer c()
		// Make sure we never leave a paused container behind.
		_ = topo.Down(dctx)
	}()

	leaderDSN := topo.ConnString()
	if leaderDSN == "" {
		t.Fatal("ConnString empty after Up")
	}
	frozen := leaderContainer(t, ctx, topo, leaderDSN)

	t.Logf("freezing leader container %s (docker pause)", frozen)
	dockerMust(t, ctx, "pause", frozen)
	// Belt-and-suspenders: if anything below fails, still unpause so Down
	// can tear the stack down cleanly.
	unpaused := false
	defer func() {
		if !unpaused {
			uctx, uc := context.WithTimeout(context.Background(), 30*time.Second)
			defer uc()
			_ = exec.CommandContext(uctx, "docker", "unpause", frozen).Run()
		}
	}()

	newDSN := waitForNewWritableLeader(t, ctx, topo, leaderDSN, 150*time.Second)
	t.Logf("frozen leader fenced; new leader at %s", newDSN)

	t.Logf("thawing old leader %s (docker unpause)", frozen)
	dockerMust(t, ctx, "unpause", frozen)
	unpaused = true

	// The thawed ex-leader must notice it lost leadership, demote, and
	// rejoin — leaving a healthy 3-member cluster with a single leader.
	waitForHealthyCluster(t, ctx, topo, 3, 4*time.Minute)
}

// patroniSuperPwd mirrors the topology's baked-in superuser password
// (unexported there); used to build DSNs to individual nodes by port.
const patroniSuperPwd = "testkit"

// dataIntegrityDigest is a deterministic, order-independent fingerprint of
// the whole hs_data_t table — count plus an md5 over id=v pairs sorted by
// id. Identical content on any node yields the identical string, so it
// detects row loss, extra rows, or any value corruption.
const dataIntegrityDigest = `SELECT count(*)::text || ':' || ` +
	`coalesce(md5(string_agg(id::text || '=' || v, ',' ORDER BY id)), '') FROM hs_data_t`

// seedDataset writes a deterministic, content-addressed dataset
// (id, md5(id)) for ids [from,to] into hs_data_t on dsn, then checkpoints.
func seedDataset(t *testing.T, ctx context.Context, dsn string, from, to int) {
	t.Helper()
	reg := mustConnect(t, ctx, dsn, pg.ModeRegular)
	defer reg.Close(context.Background())
	regExec(t, ctx, reg, "CREATE TABLE IF NOT EXISTS hs_data_t (id int PRIMARY KEY, v text)")
	regExec(t, ctx, reg, fmt.Sprintf("INSERT INTO hs_data_t SELECT g, md5(g::text) FROM generate_series(%d,%d) g", from, to))
	regExec(t, ctx, reg, "CHECKPOINT")
}

// scalar runs a single-value query via database/sql and returns it,
// failing the test on error.
func scalar(t *testing.T, ctx context.Context, dsn, query string) string {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	qctx, qc := context.WithTimeout(ctx, 30*time.Second)
	defer qc()
	var s string
	if err := db.QueryRowContext(qctx, query).Scan(&s); err != nil {
		t.Fatalf("scalar %q on %s: %v", query, dsn, err)
	}
	return s
}

// scalarSoft is scalar without fatals — returns "" on any error. Used when
// polling a node that may be transiently unavailable (mid-rejoin).
func scalarSoft(ctx context.Context, dsn, query string) string {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return ""
	}
	defer db.Close()
	qctx, qc := context.WithTimeout(ctx, 10*time.Second)
	defer qc()
	var s string
	if err := db.QueryRowContext(qctx, query).Scan(&s); err != nil {
		return ""
	}
	return s
}

// dsnForContainer builds a libpq DSN to the PostgreSQL inside a specific
// container via its published 5432 mapping. Lets a test read a particular
// node (e.g. a rejoined replica) directly rather than via leader discovery.
func dsnForContainer(t *testing.T, ctx context.Context, container string) string {
	t.Helper()
	out, err := exec.CommandContext(ctx, "docker", "port", container, "5432/tcp").CombinedOutput()
	if err != nil {
		t.Fatalf("docker port %s: %v\n%s", container, err, out)
	}
	port := hostPortOf(string(out))
	if port == "" {
		t.Fatalf("no published host port for %s (%q)", container, out)
	}
	return fmt.Sprintf("postgres://postgres:%s@127.0.0.1:%s/postgres?sslmode=disable", patroniSuperPwd, port)
}

// TestPatroniDataIntegrity_AcrossHardFailoverAndRejoin is the data-quality
// centrepiece: it proves the data we'd recover is correct, not merely that
// the cluster survives.
//
//  1. Seed a deterministic dataset and fingerprint it.
//  2. SIGKILL the leader; a replica is promoted.
//  3. The promoted leader's fingerprint MUST equal the original — no
//     committed row may be lost or corrupted by the failover.
//  4. Write more data, refingerprint.
//  5. Revive the dead node; read it DIRECTLY once it rejoins and require its
//     fingerprint to converge to the leader's — the recovered node holds the
//     exact correct data, not a stale or diverged copy.
func TestPatroniDataIntegrity_AcrossHardFailoverAndRejoin(t *testing.T) {
	topo, err := topology.Build("patroni-local-docker")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 14*time.Minute)
	defer cancel()
	if err := topo.Up(ctx, topology.UpOptions{}); err != nil {
		t.Fatalf("Up: %v", err)
	}
	defer func() {
		dctx, c := context.WithTimeout(context.Background(), 90*time.Second)
		defer c()
		_ = topo.Down(dctx)
	}()

	leaderDSN := topo.ConnString()
	if leaderDSN == "" {
		t.Fatal("ConnString empty after Up")
	}

	// 1. Seed + fingerprint.
	seedDataset(t, ctx, leaderDSN, 1, 5000)
	d1 := scalar(t, ctx, leaderDSN, dataIntegrityDigest)
	t.Logf("seeded 5000 rows; leader digest=%s", d1)

	// 2. Hard failover.
	dead := leaderContainer(t, ctx, topo, leaderDSN)
	t.Logf("killing leader %s", dead)
	dockerMust(t, ctx, "kill", dead)
	newDSN := waitForNewWritableLeader(t, ctx, topo, leaderDSN, 150*time.Second)

	// 3. THE assertion: committed data is byte-identical on the new leader.
	d1b := scalar(t, ctx, newDSN, dataIntegrityDigest)
	if d1b != d1 {
		t.Fatalf("DATA LOSS/CORRUPTION across failover: pre=%s post=%s", d1, d1b)
	}
	t.Logf("data intact across failover: %s", d1b)

	// 4. More writes on the new leader.
	seedDataset(t, ctx, newDSN, 5001, 10000)
	d2 := scalar(t, ctx, newDSN, dataIntegrityDigest)
	t.Logf("post-failover writes; leader digest=%s", d2)

	// 5. Revive the dead node and require it to recover the CORRECT data.
	t.Logf("reviving %s", dead)
	dockerMust(t, ctx, "start", dead)
	waitForHealthyCluster(t, ctx, topo, 3, 4*time.Minute)

	repDSN := dsnForContainer(t, ctx, dead)
	deadline := time.Now().Add(3 * time.Minute)
	var got string
	for time.Now().Before(deadline) {
		got = scalarSoft(ctx, repDSN, dataIntegrityDigest)
		if got == d2 {
			t.Logf("rejoined node converged to the correct data: %s", got)
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("ctx cancelled waiting for rejoined-node convergence (want %s, got %q)", d2, got)
		case <-time.After(3 * time.Second):
		}
	}
	t.Fatalf("rejoined node did NOT converge to correct data within 3m: want %s, got %q", d2, got)
}

// TestPatroniDataIntegrity_AcrossSwitchover is the graceful-switchover
// counterpart: a planned handoff must preserve every committed row exactly,
// and the new leader must accept and durably hold further writes.
func TestPatroniDataIntegrity_AcrossSwitchover(t *testing.T) {
	topo, err := topology.Build("patroni-local-docker")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	if err := topo.Up(ctx, topology.UpOptions{}); err != nil {
		t.Fatalf("Up: %v", err)
	}
	defer func() {
		dctx, c := context.WithTimeout(context.Background(), 90*time.Second)
		defer c()
		_ = topo.Down(dctx)
	}()

	leaderDSN := topo.ConnString()
	if leaderDSN == "" {
		t.Fatal("ConnString empty after Up")
	}

	seedDataset(t, ctx, leaderDSN, 1, 8000)
	d1 := scalar(t, ctx, leaderDSN, dataIntegrityDigest)
	t.Logf("seeded 8000 rows; digest=%s", d1)

	newDSN := switchover(t, ctx, topo, leaderDSN)

	d1b := scalar(t, ctx, newDSN, dataIntegrityDigest)
	if d1b != d1 {
		t.Fatalf("DATA LOSS/CORRUPTION across switchover: pre=%s post=%s", d1, d1b)
	}
	t.Logf("data intact across switchover: %s", d1b)

	// New leader must accept + durably keep further writes.
	seedDataset(t, ctx, newDSN, 8001, 12000)
	d2 := scalar(t, ctx, newDSN, dataIntegrityDigest)
	want := scalar(t, ctx, newDSN, "SELECT count(*)::text FROM hs_data_t")
	if want != "12000" {
		t.Errorf("expected 12000 rows after post-switchover writes, got %s", want)
	}
	t.Logf("post-switchover writes durable; digest=%s rows=%s", d2, want)
}
