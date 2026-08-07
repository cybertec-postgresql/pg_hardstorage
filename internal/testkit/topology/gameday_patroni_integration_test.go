// gameday_patroni_integration_test.go — the game-day scenario, driven
// against a real 3-node Patroni cluster.
//
// This is what makes `gameday run patroni_failover` a test rather than
// a promise. The scenario used to append evidence saying "runtime drive
// lands alongside the verifier sandbox's owned Patroni cluster" and
// return Pass=true — a tier-L4 drill that exited 0 having promoted
// nothing. It now drives a real switchover through internal/patroni,
// waits for a different member to take the leader lock, and fails if
// the leader never moves.
//
// Everything below goes through the SAME seam the CLI uses
// (gameday.PatroniDriver), so what CI proves here is what an operator
// gets when they point the command at their own cluster. A test that
// drove Patroni some other way would prove something about the test.
//
//go:build integration && patroni

package topology_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/gameday"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/patroni"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/replication"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/topology"
)

// realDriver adapts internal/patroni to the scenario's seam. It is a
// deliberate copy of the CLI's adapter rather than an import: the CLI's
// lives in package cli behind config loading, and duplicating six lines
// is cheaper than exporting plumbing just for a test.
type realDriver struct{ c *patroni.Client }

func (d realDriver) Leader(ctx context.Context) (string, uint32, error) {
	m, err := d.c.Leader(ctx)
	if err != nil {
		return "", 0, err
	}
	return m.Name, m.Timeline, nil
}

func (d realDriver) Switchover(ctx context.Context, leader string) error {
	return d.c.Switchover(ctx, patroni.SwitchoverRequest{Leader: leader})
}

// upPatroniCluster brings up the shared 3-node cluster and returns a
// driver pointed at a member that answers.
func upPatroniCluster(t *testing.T) (gameday.PatroniDriver, topology.Topology, func()) {
	t.Helper()
	topo, err := topology.Build("patroni-local-docker")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	if err := topo.Up(ctx, topology.UpOptions{}); err != nil {
		t.Fatalf("Up: %v", err)
	}
	down := func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer dcancel()
		_ = topo.Down(dctx)
	}

	cluster, ok := topo.(topology.PatroniCluster)
	if !ok {
		down()
		t.Fatal("patroni-local-docker does not implement PatroniCluster; the scenario " +
			"cannot be driven through the product's own REST client")
	}
	urls := cluster.PatroniRESTURLs()
	if len(urls) == 0 {
		down()
		t.Fatal("cluster published no Patroni REST endpoints")
	}

	// Any member answers /cluster; pick the first that does.
	probe, pcancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer pcancel()
	for _, u := range urls {
		c, err := patroni.NewClient(u)
		if err != nil {
			continue
		}
		if _, err := c.Leader(probe); err == nil {
			return realDriver{c: c}, topo, down
		}
	}
	down()
	t.Fatalf("no Patroni member answered /cluster on %v", urls)
	return nil, nil, nil
}

// realObserveSlot builds the ObserveSlot seam against the live cluster,
// so the drill measures its declared invariant instead of asserting
// only that the leader moved.
//
// The scenario calls this twice: once as a baseline before the
// switchover, once after. The first call captures the leader's current
// WAL position and creates the slot; the second runs against whoever is
// now the leader — ConnString re-resolves on every call — and lets
// EnsureSlot compute the gap between the position we confirmed and
// where the slot now starts.
//
// The captured LSN is what makes the measurement real. Passing zero
// makes EnsureSlot skip the gap calculation entirely, which is the
// production bug fixed in 060b64c: the drill would report GapBytes=0
// having compared nothing.
func realObserveSlot(t *testing.T, topo topology.Topology, slotName string) func(context.Context) (*gameday.SlotObservation, error) {
	t.Helper()
	var confirmed pglogrepl.LSN

	return func(ctx context.Context) (*gameday.SlotObservation, error) {
		dsn := topo.ConnString()
		if dsn == "" {
			return nil, fmt.Errorf("topology published no DSN")
		}
		regConn, err := pg.Connect(ctx, dsn, pg.ModeRegular)
		if err != nil {
			return nil, fmt.Errorf("connect (regular): %w", err)
		}
		defer regConn.Close(ctx)
		replConn, err := pg.Connect(ctx, dsn, pg.ModeReplication)
		if err != nil {
			return nil, fmt.Errorf("connect (replication): %w", err)
		}
		defer replConn.Close(ctx)

		if confirmed == 0 {
			rows, qerr := regConn.PgConn().Exec(ctx, "SELECT pg_current_wal_lsn()::text").ReadAll()
			if qerr != nil {
				return nil, fmt.Errorf("read current wal lsn: %w", qerr)
			}
			for _, r := range rows {
				for _, row := range r.Rows {
					if len(row) > 0 && row[0] != nil {
						lsn, perr := pglogrepl.ParseLSN(string(row[0]))
						if perr != nil {
							return nil, fmt.Errorf("parse current wal lsn %q: %w", row[0], perr)
						}
						confirmed = lsn
					}
				}
			}
			if confirmed == 0 {
				return nil, fmt.Errorf("could not read the leader's current WAL position")
			}
			t.Logf("baseline confirmed LSN: %s", confirmed)
		}

		res, eerr := replication.EnsureSlot(ctx, regConn, replConn, slotName, confirmed)
		if eerr != nil {
			return nil, fmt.Errorf("ensure slot %q: %w", slotName, eerr)
		}
		return &gameday.SlotObservation{
			Outcome:  string(res.Outcome),
			GapBytes: res.GapBytes,
		}, nil
	}
}

// TestGameDay_PatroniFailover_DrivesARealSwitchover is the end-to-end
// proof, in both directions, on one cluster.
//
// The drill's declared invariant is that a planned switchover must not
// COST us WAL, so proving it works means proving it can FAIL as well as
// pass. Those two states are a cluster-configuration apart:
//
//   - a plain physical slot does not survive a Patroni switchover. The
//     reconciler finds it gone on the new leader and recreates it at
//     that leader's current position, which strands every byte between
//     the last confirmed LSN and there. That is Strategy C, and it is
//     real WAL loss.
//   - a slot declared in Patroni's `permanent_slots` is re-established
//     by Patroni on the promoted leader, so the reconciler FINDS it and
//     nothing is stranded. That is Mechanism 1.
//
// Phase 1 runs the drill on the cluster as it boots — no permanent
// slots — and requires the drill to REPORT the loss. Phase 2 declares
// the slot permanent and requires the drill to pass. A drill that can
// only do one of those is not evidence of anything.
//
// This is what the scenario could not do at all until ObserveSlot was
// wired: it took the unmeasured branch, asserted only that the leader
// moved, and returned Pass=true on both clusters alike.
func TestGameDay_PatroniFailover_DrivesARealSwitchover(t *testing.T) {
	drv, topo, down := upPatroniCluster(t)
	defer down()

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	before, _, err := drv.Leader(ctx)
	if err != nil {
		t.Fatalf("read leader before the drill: %v", err)
	}

	// --- Phase 1: a plain slot, which cannot survive the promotion ---
	res, err := gameday.Run(ctx, "patroni_failover", gameday.RunOptions{
		Deployment:    "db1",
		Patroni:       drv,
		RecoverWithin: 3 * time.Minute,
		ObserveSlot:   realObserveSlot(t, topo, "gameday_drill_plain"),
	})
	if err != nil {
		t.Fatalf("gameday.Run (plain slot): %v", err)
	}
	if res.RecoveryTime <= 0 {
		t.Error("no RecoveryTime recorded; the drill cannot report how long promotion took")
	}
	if res.Deferred {
		t.Errorf("scenario reported Deferred while driving a real cluster with ObserveSlot "+
			"wired: %s", res.Failure)
	}
	if res.Pass {
		t.Errorf("the drill PASSED on a cluster with no permanent_slots.\n\n"+
			"A plain physical slot does not survive a Patroni switchover: the reconciler "+
			"recreates it on the new leader at that leader's position, stranding the WAL "+
			"between. The drill exists to catch exactly that, and reporting a pass here is "+
			"the hollow pass the ObserveSlot wiring was added to remove.\nevidence: %+v",
			res.Evidence)
	} else {
		t.Logf("phase 1: drill correctly reported the loss: %s", res.Failure)
	}

	// The product-visible outcome: a different member holds the lock.
	after, _, err := drv.Leader(ctx)
	if err != nil {
		t.Fatalf("read leader after the drill: %v", err)
	}
	if after == before {
		t.Fatalf("drill ran but %q still holds the leader lock — the scenario is not "+
			"observing what it claims to observe", before)
	}
	t.Logf("phase 1: leader moved %s -> %s in %s", before, after, res.RecoveryTime)

	// --- Phase 2: the same drill on a correctly configured cluster ---
	const permSlot = "gameday_drill_perm"
	pc := firstPatroniContainer(t, topo)
	dockerExec(t, ctx, pc, "patronictl", "edit-config", "--force",
		"-s", "slots."+permSlot+".type=physical")
	// Patroni must materialise it on the current leader before the
	// baseline observation, or phase 2 measures a slot that is not yet
	// permanent and reproduces phase 1.
	_ = waitForSlotLSN(t, ctx, topo, permSlot, 90*time.Second)
	t.Logf("phase 2: permanent slot %q present on the leader", permSlot)

	res2, err := gameday.Run(ctx, "patroni_failover", gameday.RunOptions{
		Deployment:    "db1",
		Patroni:       drv,
		RecoverWithin: 3 * time.Minute,
		ObserveSlot:   realObserveSlot(t, topo, permSlot),
	})
	if err != nil {
		t.Fatalf("gameday.Run (permanent slot): %v", err)
	}
	if !res2.Pass {
		t.Errorf("the drill FAILED on a cluster whose slot is declared permanent: %s\n\n"+
			"Patroni re-establishes a permanent slot on the promoted leader, so the "+
			"reconciler should find it and strand nothing. A failure here means either "+
			"Mechanism 1 is not working or the drill is reporting loss that did not "+
			"happen — and a drill that cries wolf gets switched off.\nevidence: %+v",
			res2.Failure, res2.Evidence)
	} else {
		t.Logf("phase 2: drill passed on a correctly configured cluster in %s", res2.RecoveryTime)
	}

	final, _, err := drv.Leader(ctx)
	if err != nil {
		t.Fatalf("read leader after phase 2: %v", err)
	}
	if final == after {
		t.Errorf("phase 2 did not move the leader off %q", after)
	}
}

// TestGameDay_PatroniFailover_RefusesAnUnreachableCluster pins the
// other half: a drill that cannot reach Patroni must FAIL, not pass.
// An unreachable endpoint is the single most likely way this scenario
// silently stops testing anything.
func TestGameDay_PatroniFailover_RefusesAnUnreachableCluster(t *testing.T) {
	c, err := patroni.NewClient("http://127.0.0.1:1")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res, err := gameday.Run(ctx, "patroni_failover", gameday.RunOptions{
		Patroni:       realDriver{c: c},
		RecoverWithin: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("gameday.Run: %v", err)
	}
	if res.Pass {
		t.Fatal("drill passed against an unreachable Patroni endpoint")
	}
	if res.Misconfigured {
		t.Error("an endpoint that is configured but unreachable is not a misconfiguration; " +
			"reporting it as one sends the operator to their YAML instead of their network")
	}
	if res.Failure == "" {
		t.Error("drill failed with no explanation; an operator gets a non-zero exit and " +
			"nothing to act on")
	}
}
