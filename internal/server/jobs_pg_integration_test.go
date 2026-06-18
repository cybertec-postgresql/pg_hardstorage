//go:build integration

package server_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	pgtestkit "github.com/cybertec-postgresql/pg_hardstorage/internal/pg/testkit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/server"
)

// TestPGBackend_Contract runs the same shared contract as the memory
// backend, but against a real PostgreSQL spun up via testcontainers.
// Build-tagged `integration` so the default `go test ./...` run stays
// Docker-free; CI's integration tier runs `go test -tags=integration ./...`.
//
// Each subtest gets a fresh schema-bootstrapped pool: the contract
// suite calls factory(t) once per subtest and we wipe phs.jobs
// between subtests so they don't see each other's queue state.
func TestPGBackend_Contract(t *testing.T) {
	pg := pgtestkit.StartPostgres(t)

	runBackendContract(t, func(t *testing.T) server.JobBackend {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		b, err := server.OpenPGBackend(ctx, pg.DSN)
		if err != nil {
			t.Fatalf("OpenPGBackend: %v", err)
		}
		// Wipe any rows from the prior subtest. The schema is
		// idempotent (CREATE IF NOT EXISTS) so the second OpenPGBackend
		// reuses the existing tables; we just need a clean queue.
		if _, err := b.Pool().Exec(ctx, `TRUNCATE phs.jobs`); err != nil {
			t.Fatalf("truncate: %v", err)
		}
		t.Cleanup(func() { _ = b.Close() })
		return b
	})
}

// TestPGBackend_ConcurrentClaim exercises the FOR UPDATE SKIP LOCKED
// claim semantics. Two goroutines race for the same queued job; one
// must win, the other must get ErrNoJobs. This is the whole reason
// the PG backend exists — without atomic claim, multi-control-plane
// dispatch would double-execute jobs.
func TestPGBackend_ConcurrentClaim(t *testing.T) {
	pg := pgtestkit.StartPostgres(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b, err := server.OpenPGBackend(ctx, pg.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	if _, err := b.Pool().Exec(ctx, `TRUNCATE phs.jobs`); err != nil {
		t.Fatal(err)
	}

	// One queued job.
	if _, err := b.Enqueue(ctx, server.EnqueueOptions{
		Kind:       server.JobBackup,
		Deployment: "db1",
	}); err != nil {
		t.Fatal(err)
	}

	// Race two claims. Use a barrier so both goroutines hit the
	// query at roughly the same time — without it, the first one
	// would always finish before the second even started.
	const N = 16
	var (
		wg      sync.WaitGroup
		barrier = make(chan struct{})

		mu       sync.Mutex
		wins     int
		nojobs   int
		othererr int
	)
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			<-barrier
			_, err := b.Claim(ctx, server.ClaimOptions{
				AgentID:     "racer",
				Deployments: []string{"db1"},
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case errors.Is(err, server.ErrNoJobs):
				nojobs++
			default:
				othererr++
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	close(barrier)
	wg.Wait()

	if wins != 1 {
		t.Errorf("wins = %d, want exactly 1 (FOR UPDATE SKIP LOCKED contract)", wins)
	}
	if nojobs != N-1 {
		t.Errorf("nojobs = %d, want %d (every loser must get ErrNoJobs)", nojobs, N-1)
	}
	if othererr != 0 {
		t.Errorf("othererr = %d, want 0", othererr)
	}
}

// TestPGBackend_ConcurrencyCapIsHard pins race-condition audit #5: under
// CONCURRENT claims, the cap is a HARD limit — exactly `cap` claims win,
// never more. The advisory-lock serialization makes the running-count
// check atomic with the claim, so two control planes can't both observe a
// pre-claim count below the cap and overshoot it (the old soft cap could).
func TestPGBackend_ConcurrencyCapIsHard(t *testing.T) {
	pg := pgtestkit.StartPostgres(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b, err := server.OpenPGBackend(ctx, pg.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	if _, err := b.Pool().Exec(ctx, `TRUNCATE phs.jobs`); err != nil {
		t.Fatal(err)
	}

	const cap = 3
	const N = 16
	for i := 0; i < N; i++ {
		if _, err := b.Enqueue(ctx, server.EnqueueOptions{Kind: server.JobBackup, Deployment: "db1"}); err != nil {
			t.Fatal(err)
		}
	}

	// Fire N concurrent capped claims behind a barrier so they all hit the
	// count-and-claim at once — exactly the burst the soft cap overshot.
	var (
		wg      sync.WaitGroup
		barrier = make(chan struct{})
		mu      sync.Mutex
		wins    int
	)
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			<-barrier
			j, err := b.Claim(ctx, server.ClaimOptions{
				AgentID:       fmt.Sprintf("racer-%d", i),
				Deployments:   []string{"db1"},
				MaxConcurrent: cap,
			})
			switch {
			case err == nil && j != nil:
				mu.Lock()
				wins++
				mu.Unlock()
			case errors.Is(err, server.ErrNoJobs):
				// cap reached — expected for the losers
			default:
				t.Errorf("unexpected claim error: %v", err)
			}
		}(i)
	}
	close(barrier)
	wg.Wait()

	if wins != cap {
		t.Fatalf("hard cap: %d claims won, want exactly %d (running count must never exceed the cap)", wins, cap)
	}
	running, err := b.List(ctx, server.ListOptions{State: server.JobRunning})
	if err != nil {
		t.Fatal(err)
	}
	if len(running) != cap {
		t.Errorf("running jobs = %d, want %d", len(running), cap)
	}
}

// TestPGBackend_ConcurrencyCap validates the SQL concurrency guard
// against a real PostgreSQL: sequential claims with MaxConcurrent=K
// succeed K times, are then refused (ErrNoJobs) while at the cap, and
// resume once a running job completes. Sequential claims each observe
// the committed running count, so the cap is exact here.
func TestPGBackend_ConcurrencyCap(t *testing.T) {
	pg := pgtestkit.StartPostgres(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b, err := server.OpenPGBackend(ctx, pg.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	if _, err := b.Pool().Exec(ctx, `TRUNCATE phs.jobs`); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 5; i++ {
		if _, err := b.Enqueue(ctx, server.EnqueueOptions{Kind: server.JobBackup, Deployment: "db1"}); err != nil {
			t.Fatal(err)
		}
	}

	const cap = 2
	opts := server.ClaimOptions{AgentID: "a", Deployments: []string{"db1"}, MaxConcurrent: cap}

	first, err := b.Claim(ctx, opts)
	if err != nil {
		t.Fatalf("claim 1: %v", err)
	}
	if _, err := b.Claim(ctx, opts); err != nil {
		t.Fatalf("claim 2: %v", err)
	}
	// At the cap (2 running) → refused even though 3 remain queued.
	if _, err := b.Claim(ctx, opts); !errors.Is(err, server.ErrNoJobs) {
		t.Fatalf("claim at cap: got %v, want ErrNoJobs", err)
	}
	// Free a slot.
	if _, err := b.Complete(ctx, first.ID, server.CompleteOptions{Success: true}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if _, err := b.Claim(ctx, opts); err != nil {
		t.Fatalf("claim after slot freed: %v", err)
	}
	if _, err := b.Claim(ctx, opts); !errors.Is(err, server.ErrNoJobs) {
		t.Fatalf("claim at cap again: got %v, want ErrNoJobs", err)
	}
}

// TestPGBackend_BootstrapIsIdempotent checks the documented
// "open against an already-bootstrapped DB is a no-op" contract: a
// second control plane joining must not re-create or trip on existing
// schema objects.
func TestPGBackend_BootstrapIsIdempotent(t *testing.T) {
	pg := pgtestkit.StartPostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	first, err := server.OpenPGBackend(ctx, pg.DSN)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	defer func() { _ = first.Close() }()

	// Second instance — schema must be a no-op (every CREATE uses
	// IF NOT EXISTS) and we should be able to read what the first
	// wrote.
	second, err := server.OpenPGBackend(ctx, pg.DSN)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer func() { _ = second.Close() }()

	j, err := first.Enqueue(ctx, server.EnqueueOptions{
		Kind:       server.JobBackup,
		Deployment: "db1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.Get(ctx, j.ID); err != nil {
		t.Errorf("second instance can't see job from first: %v", err)
	}
}
