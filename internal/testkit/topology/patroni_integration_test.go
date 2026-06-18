// Build-tagged so default `go test ./...` skips the heavy
// 1+ GB Spilo image pull and ~30 s cluster bring-up.
//
// Run with:
//
//	make test-integration   (or)   go test -tags integration ./internal/testkit/topology/...

//go:build integration

package topology_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/inject"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/topology"
)

// TestPatroniLocalDocker_LifecycleAndFailover covers the
// load-bearing contract:
//
//  1. Up brings up etcd + 3 Spilo nodes, blocks until a leader
//     is elected.
//  2. ConnString returns a libpq DSN that reaches the current
//     leader and accepts a SELECT.
//  3. Targets surfaces 3 patroni-role docker targets + 1 etcd.
//  4. After a patroni_switchover fault, ConnString eventually
//     reflects the new leader (the leader pg port is different
//     from the pre-switchover port).
//
// Skipped in the default test run (go:build integration).
func TestPatroniLocalDocker_LifecycleAndFailover(t *testing.T) {
	topo, err := topology.Build("patroni-local-docker")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	if err := topo.Up(ctx, topology.UpOptions{}); err != nil {
		t.Fatalf("Up: %v", err)
	}
	defer func() {
		downCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_ = topo.Down(downCtx)
	}()

	// 2. DSN reaches the leader.
	dsn := topo.ConnString()
	if dsn == "" {
		t.Fatal("ConnString returned empty after Up")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	pingCtx, pcancel := context.WithTimeout(ctx, 30*time.Second)
	defer pcancel()
	if err := db.PingContext(pingCtx); err != nil {
		t.Fatalf("ping leader: %v", err)
	}
	var pgIsInRecovery bool
	if err := db.QueryRowContext(pingCtx, "SELECT pg_is_in_recovery()").Scan(&pgIsInRecovery); err != nil {
		t.Fatalf("pg_is_in_recovery query: %v", err)
	}
	if pgIsInRecovery {
		t.Errorf("ConnString returned a replica (pg_is_in_recovery=true); leader discovery is broken")
	}

	// 3. Targets surface = 3 patroni + 1 etcd.
	tgs := topo.Targets()
	patroniCount, etcdCount := 0, 0
	for _, t := range tgs {
		switch t.Role() {
		case "patroni":
			patroniCount++
		case "etcd":
			etcdCount++
		}
	}
	if patroniCount != 3 {
		t.Errorf("expected 3 patroni-role targets; got %d", patroniCount)
	}
	if etcdCount != 1 {
		t.Errorf("expected 1 etcd-role target; got %d", etcdCount)
	}

	// 4. Capture the pre-switchover leader DSN, fire a
	//    patroni_switchover fault via the inject registry,
	//    then poll ConnString until it points at a different
	//    PG port (= a different node).  patroni_switchover
	//    against `patroni` (all-of-role) hits every node's
	//    REST API — Patroni picks a healthy replica to
	//    promote and demotes the current leader.
	preDSN := topo.ConnString()
	ts := inject.NewStaticTargetSet(tgs, time.Now().UnixNano())
	if _, err := inject.DefaultRegistry.Apply(ctx,
		"patroni_switchover(target=patroni)", ts); err != nil {
		t.Fatalf("apply patroni_switchover: %v", err)
	}
	// During the switchover transition window Patroni's
	// /leader endpoint can flip to 200 on the candidate before
	// PG has finished promotion (pg_is_in_recovery briefly
	// stays true).  Poll past that race: we want a NEW DSN
	// AND the connection to report a writable primary.
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		cur := topo.ConnString()
		if cur == "" || cur == preDSN {
			select {
			case <-ctx.Done():
				t.Fatal("ctx cancelled while waiting for switchover")
			case <-time.After(2 * time.Second):
			}
			continue
		}
		// New endpoint reachable — confirm it's a writable
		// primary, not a replica still finishing promotion.
		db2, err := sql.Open("pgx", cur)
		if err != nil {
			t.Fatalf("sql.Open(post-switchover): %v", err)
		}
		var rec bool
		pingCtx2, pcancel2 := context.WithTimeout(ctx, 5*time.Second)
		err = db2.QueryRowContext(pingCtx2, "SELECT pg_is_in_recovery()").Scan(&rec)
		pcancel2()
		_ = db2.Close()
		if err == nil && !rec {
			return // new primary is writable; happy
		}
		// Still mid-promotion — give Patroni a beat and retry.
		select {
		case <-ctx.Done():
			t.Fatal("ctx cancelled while waiting for promotion")
		case <-time.After(2 * time.Second):
		}
	}
	t.Errorf("post-switchover primary did not become writable within 60s (preDSN=%q, currentDSN=%q)",
		preDSN, topo.ConnString())
}
