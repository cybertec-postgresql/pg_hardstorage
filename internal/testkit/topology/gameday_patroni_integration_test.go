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
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/gameday"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/patroni"
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
func upPatroniCluster(t *testing.T) (gameday.PatroniDriver, func()) {
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
			return realDriver{c: c}, down
		}
	}
	down()
	t.Fatalf("no Patroni member answered /cluster on %v", urls)
	return nil, nil
}

// TestGameDay_PatroniFailover_DrivesARealSwitchover is the end-to-end
// proof: the scenario the CLI runs, against a real cluster, asserting
// the leader actually moved.
func TestGameDay_PatroniFailover_DrivesARealSwitchover(t *testing.T) {
	drv, down := upPatroniCluster(t)
	defer down()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	before, _, err := drv.Leader(ctx)
	if err != nil {
		t.Fatalf("read leader before the drill: %v", err)
	}

	res, err := gameday.Run(ctx, "patroni_failover", gameday.RunOptions{
		Deployment:    "db1",
		Patroni:       drv,
		RecoverWithin: 3 * time.Minute,
	})
	if err != nil {
		t.Fatalf("gameday.Run: %v", err)
	}
	if !res.Pass {
		t.Fatalf("drill failed against a healthy 3-node cluster: %s\nevidence: %+v",
			res.Failure, res.Evidence)
	}
	if res.Deferred {
		t.Error("scenario reported Deferred while driving a real cluster — the runtime " +
			"drive IS implemented, so this flag is now a lie")
	}
	if res.RecoveryTime <= 0 {
		t.Error("no RecoveryTime recorded; the drill cannot report how long promotion took")
	}

	// The product-visible outcome: a different member holds the lock.
	after, _, err := drv.Leader(ctx)
	if err != nil {
		t.Fatalf("read leader after the drill: %v", err)
	}
	if after == before {
		t.Fatalf("drill passed but %q still holds the leader lock — the scenario is not "+
			"observing what it claims to observe", before)
	}
	t.Logf("leader moved %s -> %s in %s", before, after, res.RecoveryTime)
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
