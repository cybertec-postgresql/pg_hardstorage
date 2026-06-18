package server_test

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/server"
)

func enqueueN(t *testing.T, r *server.JobRegistry, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := r.Enqueue(server.EnqueueOptions{
			Kind:       server.JobBackup,
			Deployment: "db1",
			RepoURL:    "file:///srv/repo",
		}); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
}

func claim(r *server.JobRegistry, agent string) (*server.Job, error) {
	return r.Claim(server.ClaimOptions{AgentID: agent, Deployments: []string{"db1"}})
}

// TestClaim_ConcurrencyCap_RefusesAtLimit: with the cap set, claims
// succeed up to the limit, are refused (ErrNoJobs) while at the limit,
// and resume once a running job completes and frees a slot.
func TestClaim_ConcurrencyCap_RefusesAtLimit(t *testing.T) {
	r := server.NewJobRegistry().WithMaxConcurrent(2)
	enqueueN(t, r, 4)

	j1, err := claim(r, "a1")
	if err != nil {
		t.Fatalf("claim 1: %v", err)
	}
	if _, err := claim(r, "a2"); err != nil {
		t.Fatalf("claim 2: %v", err)
	}

	// At the cap (2 running) → refused, even though 2 jobs are still
	// queued.
	if _, err := claim(r, "a3"); !errors.Is(err, server.ErrNoJobs) {
		t.Fatalf("claim at cap: got %v, want ErrNoJobs", err)
	}

	// Free a slot.
	if _, err := r.Complete(j1.ID, server.CompleteOptions{Success: true}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	// Now a claim succeeds again.
	if _, err := claim(r, "a3"); err != nil {
		t.Fatalf("claim after slot freed: %v", err)
	}
	// And we're back at the cap.
	if _, err := claim(r, "a4"); !errors.Is(err, server.ErrNoJobs) {
		t.Fatalf("claim at cap again: got %v, want ErrNoJobs", err)
	}
}

// TestClaim_ConcurrencyCap_ZeroIsUnlimited: with no cap (the default),
// every queued job can be claimed.
func TestClaim_ConcurrencyCap_ZeroIsUnlimited(t *testing.T) {
	r := server.NewJobRegistry() // no WithMaxConcurrent → unlimited
	if r.MaxConcurrent() != 0 {
		t.Fatalf("default MaxConcurrent = %d, want 0", r.MaxConcurrent())
	}
	enqueueN(t, r, 5)
	for i := 0; i < 5; i++ {
		if _, err := claim(r, "a"); err != nil {
			t.Fatalf("claim %d should succeed under unlimited cap: %v", i, err)
		}
	}
}

// TestClaim_ConcurrencyCap_NegativeDisables: a negative cap is
// normalized to 0 (unlimited).
func TestClaim_ConcurrencyCap_NegativeDisables(t *testing.T) {
	r := server.NewJobRegistry().WithMaxConcurrent(-1)
	if r.MaxConcurrent() != 0 {
		t.Errorf("negative cap should normalize to 0; got %d", r.MaxConcurrent())
	}
}

// TestClaim_ConcurrencyCap_HardUnderConcurrency: the memory backend
// counts and claims under one lock, so a swarm of concurrent claims
// never exceeds the cap (no overshoot).
func TestClaim_ConcurrencyCap_HardUnderConcurrency(t *testing.T) {
	const cap = 3
	r := server.NewJobRegistry().WithMaxConcurrent(cap)
	enqueueN(t, r, 50)

	var ok, refused atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := claim(r, "agent")
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, server.ErrNoJobs):
				refused.Add(1)
			default:
				t.Errorf("unexpected claim error: %v", err)
			}
		}(i)
	}
	wg.Wait()

	if ok.Load() != cap {
		t.Errorf("claims that succeeded = %d, want exactly %d (hard cap)", ok.Load(), cap)
	}
	// Confirm exactly `cap` jobs are running.
	running := 0
	for _, j := range r.List(server.ListOptions{}) {
		if j.State == server.JobRunning {
			running++
		}
	}
	if running != cap {
		t.Errorf("running jobs = %d, want %d", running, cap)
	}
}
