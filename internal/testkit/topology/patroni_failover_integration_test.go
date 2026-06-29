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
	"fmt"
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

	// ---- Phase 5: a second switchover (failback). The recreate+gap path
	// must hold across repeated leader changes, not just the first. ----
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

		res, err := replication.EnsureSlot(ctx, reg, repl, slot, newRestart)
		if err != nil {
			t.Fatalf("EnsureSlot after second switchover: %v", err)
		}
		if res.Outcome != replication.SlotRecreated {
			t.Errorf("second switchover: Outcome = %q, want %q", res.Outcome, replication.SlotRecreated)
		}
		if !res.HasGap() {
			t.Errorf("second switchover: expected a gap relative to last_confirmed=%s", newRestart)
		}
		t.Logf("phase 5: second switchover again lost the slot and reported a gap (bytes=%d)", res.GapBytes)
	}()
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
	deadline := time.Now().Add(90 * time.Second)
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
					t.Logf("switchover complete: new leader %s", cur)
					return cur
				}
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("ctx cancelled waiting for switchover (prev=%s, cur=%s)", prevDSN, topo.ConnString())
		case <-time.After(2 * time.Second):
		}
	}
	t.Fatalf("no new writable primary within 90s of switchover (prev=%s, cur=%s)", prevDSN, topo.ConnString())
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
