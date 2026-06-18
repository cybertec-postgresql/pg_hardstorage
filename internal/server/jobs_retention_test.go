package server_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/server"
)

// enqueueClaimComplete drives one job to a terminal (Completed) state
// and returns its ID.
func enqueueClaimComplete(t *testing.T, r *server.JobRegistry, dep string) string {
	t.Helper()
	j, err := r.Enqueue(server.EnqueueOptions{Kind: server.JobBackup, Deployment: dep, RepoURL: "file:///srv/repo"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Claim(server.ClaimOptions{AgentID: "agent-1", Deployments: []string{dep}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Complete(j.ID, server.CompleteOptions{Success: true}); err != nil {
		t.Fatal(err)
	}
	return j.ID
}

// TestMemoryBackend_PruneTerminal pins the leak fix (memory-leak audit
// #2): terminal jobs older than the retention window are removed from
// the in-memory map, while fresh terminal jobs and non-terminal jobs are
// kept. Without pruning the map grows for the life of the process.
func TestMemoryBackend_PruneTerminal(t *testing.T) {
	b := server.NewMemoryBackend()
	r := server.NewJobRegistryWithBackend(b)
	ctx := context.Background()

	// Three completed jobs.
	for i := 0; i < 3; i++ {
		enqueueClaimComplete(t, r, fmt.Sprintf("db%d", i))
	}
	// One still queued (never pruned, regardless of age).
	if _, err := r.Enqueue(server.EnqueueOptions{Kind: server.JobBackup, Deployment: "queued", RepoURL: "file:///srv/repo"}); err != nil {
		t.Fatal(err)
	}

	// Fresh terminal jobs are within the window — a generous retention
	// prunes nothing.
	if n, err := b.PruneTerminal(ctx, time.Hour); err != nil || n != 0 {
		t.Fatalf("PruneTerminal(1h) on fresh jobs = (%d, %v), want (0, nil)", n, err)
	}

	// Let the completed jobs age past a tiny window, then prune.
	time.Sleep(25 * time.Millisecond)
	n, err := b.PruneTerminal(ctx, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("PruneTerminal pruned %d, want 3 (the completed jobs)", n)
	}

	// The queued job survives; the completed ones are gone.
	all := r.List(server.ListOptions{})
	if len(all) != 1 || all[0].Deployment != "queued" {
		t.Fatalf("after prune, remaining jobs = %+v, want only the queued one", all)
	}
}

// TestMemoryBackend_PruneTerminal_KeepsRunning: a running job is never
// pruned even when far older than the window (it isn't terminal).
func TestMemoryBackend_PruneTerminal_KeepsRunning(t *testing.T) {
	b := server.NewMemoryBackend()
	r := server.NewJobRegistryWithBackend(b)

	j, err := r.Enqueue(server.EnqueueOptions{Kind: server.JobBackup, Deployment: "db1", RepoURL: "file:///srv/repo"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Claim(server.ClaimOptions{AgentID: "a", Deployments: []string{"db1"}}); err != nil {
		t.Fatal(err)
	}

	time.Sleep(25 * time.Millisecond)
	if n, err := b.PruneTerminal(context.Background(), time.Millisecond); err != nil || n != 0 {
		t.Fatalf("PruneTerminal removed a running job: (%d, %v)", n, err)
	}
	if _, err := r.Get(j.ID); err != nil {
		t.Errorf("running job should still exist: %v", err)
	}
}

// TestJobRegistry_PruneTerminal_RetentionDisabled: a non-positive
// retention disables pruning (the pre-fix retain-forever behaviour),
// exercised through the registry facade the sweeper calls.
func TestJobRegistry_PruneTerminal_RetentionDisabled(t *testing.T) {
	b := server.NewMemoryBackend()
	r := server.NewJobRegistryWithBackend(b).WithTerminalRetention(-1)

	enqueueClaimComplete(t, r, "db1")
	time.Sleep(25 * time.Millisecond)

	if n := r.PruneTerminal(); n != 0 {
		t.Fatalf("retention disabled but PruneTerminal pruned %d", n)
	}
	if len(r.List(server.ListOptions{})) != 1 {
		t.Fatalf("disabled retention must keep the completed job")
	}

	// Re-enable a tiny retention through the facade → it now prunes.
	r.WithTerminalRetention(time.Millisecond)
	if n := r.PruneTerminal(); n != 1 {
		t.Fatalf("facade PruneTerminal pruned %d, want 1", n)
	}
}

// TestMemoryBackend_ProgressCapped pins memory-leak audit #3: a job that
// emits far more progress events than the cap retains only the most
// recent ones, records how many were dropped, and keeps the newest event
// — so a long-running job's Progress slice can't grow without bound.
func TestMemoryBackend_ProgressCapped(t *testing.T) {
	r := server.NewJobRegistry()
	j, err := r.Enqueue(server.EnqueueOptions{Kind: server.JobBackup, Deployment: "db1", RepoURL: "file:///srv/repo"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Claim(server.ClaimOptions{AgentID: "a", Deployments: []string{"db1"}}); err != nil {
		t.Fatal(err)
	}

	const total = 2500
	for i := 0; i < total; i++ {
		if err := r.AppendProgress(j.ID, server.ProgressEvent{Op: fmt.Sprintf("ev-%d", i)}); err != nil {
			t.Fatalf("AppendProgress %d: %v", i, err)
		}
	}

	got, err := r.Get(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The const is 1000; assert the slice is bounded and the bookkeeping
	// adds up, without depending on the unexported constant directly.
	cap := len(got.Progress)
	if cap == 0 || cap >= total {
		t.Fatalf("Progress retained %d events for %d appends — must be bounded below the total", cap, total)
	}
	if got.ProgressDropped != int64(total-cap) {
		t.Errorf("ProgressDropped = %d, want %d (total %d - retained %d)", got.ProgressDropped, total-cap, total, cap)
	}
	// The retained window is the most RECENT events (a status poll wants
	// the tail): the last appended op must be present and last.
	last := got.Progress[len(got.Progress)-1]
	if last.Op != fmt.Sprintf("ev-%d", total-1) {
		t.Errorf("newest retained event Op = %q, want %q", last.Op, fmt.Sprintf("ev-%d", total-1))
	}
}
