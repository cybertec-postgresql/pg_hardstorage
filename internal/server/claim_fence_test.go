package server_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/server"
)

// TestComplete_SuccessAfterSweepReturnsClaimLost pins race-condition audit
// #3: when the sweeper marks a still-running job abandoned (Failed) and the
// agent then finishes successfully and reports Complete(success), the
// backend returns ErrClaimLost instead of silently no-op'ing — so the
// agent learns its claim was reclaimed rather than believing the backup
// was recorded.
func TestComplete_SuccessAfterSweepReturnsClaimLost(t *testing.T) {
	b := server.NewMemoryBackend()
	ctx := context.Background()

	j, err := b.Enqueue(ctx, server.EnqueueOptions{Kind: server.JobBackup, Deployment: "db1", RepoURL: "file:///r"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Claim(ctx, server.ClaimOptions{AgentID: "agent-1", Deployments: []string{"db1"}}); err != nil {
		t.Fatal(err)
	}

	// Sweeper reclaims it as abandoned while the agent is still running.
	time.Sleep(20 * time.Millisecond)
	if n, err := b.SweepAbandoned(ctx, time.Millisecond); err != nil || n != 1 {
		t.Fatalf("SweepAbandoned: n=%d err=%v (want 1, nil)", n, err)
	}

	// Agent finishes successfully and reports — must learn the claim is lost.
	_, err = b.Complete(ctx, j.ID, server.CompleteOptions{Success: true, Result: map[string]any{"backup_id": "db1.full.x"}})
	if !errors.Is(err, server.ErrClaimLost) {
		t.Fatalf("Complete(success) after a sweep must return ErrClaimLost; got %v", err)
	}

	// The job must stay Failed (the swept state), not be flipped to completed.
	got, gerr := b.Get(ctx, j.ID)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if got.State != server.JobFailed {
		t.Errorf("job should remain Failed after a lost-claim Complete; got %q", got.State)
	}
}

// TestComplete_IdempotentAndFailureStillWork: the fence must not break the
// legitimate idempotent paths — a repeat SUCCESS on an already-completed
// job, and a FAILURE report on a swept job, both stay no-op (no error).
func TestComplete_IdempotentAndFailureStillWork(t *testing.T) {
	b := server.NewMemoryBackend()
	ctx := context.Background()

	// Normal success, then a duplicate success report (agent retry).
	j, _ := b.Enqueue(ctx, server.EnqueueOptions{Kind: server.JobBackup, Deployment: "db1", RepoURL: "file:///r"})
	if _, err := b.Claim(ctx, server.ClaimOptions{AgentID: "a", Deployments: []string{"db1"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Complete(ctx, j.ID, server.CompleteOptions{Success: true}); err != nil {
		t.Fatalf("first complete: %v", err)
	}
	if _, err := b.Complete(ctx, j.ID, server.CompleteOptions{Success: true}); err != nil {
		t.Errorf("repeat success on an already-completed job must stay idempotent; got %v", err)
	}

	// A FAILURE report against a swept (Failed) job is idempotent.
	j2, _ := b.Enqueue(ctx, server.EnqueueOptions{Kind: server.JobBackup, Deployment: "db2", RepoURL: "file:///r"})
	if _, err := b.Claim(ctx, server.ClaimOptions{AgentID: "a", Deployments: []string{"db2"}}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if _, err := b.SweepAbandoned(ctx, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Complete(ctx, j2.ID, server.CompleteOptions{Success: false, Failure: "boom"}); err != nil {
		t.Errorf("failure report on a swept job must stay idempotent; got %v", err)
	}
}
