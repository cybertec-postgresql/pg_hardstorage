package server_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/server"
)

// runBackendContract is the shared scenario suite that every
// JobBackend implementation must pass. Memory runs unconditionally
// (TestMemoryBackend_Contract below); PG runs in
// jobs_pg_integration_test.go behind a build tag with
// testcontainers-go bringing up a real Postgres.
//
// Adding a new backend (etcd, future) is a matter of writing one
// factory and pointing it at this function — the suite covers the
// shape every JobRegistry caller depends on.
func runBackendContract(t *testing.T, factory func(t *testing.T) server.JobBackend) {
	t.Helper()

	t.Run("Enqueue_RequiresKindAndDeployment", func(t *testing.T) {
		b := factory(t)
		ctx := context.Background()
		if _, err := b.Enqueue(ctx, server.EnqueueOptions{Deployment: "db1"}); err == nil {
			t.Error("expected error when Kind missing")
		}
		if _, err := b.Enqueue(ctx, server.EnqueueOptions{Kind: server.JobBackup}); err == nil {
			t.Error("expected error when Deployment missing")
		}
	})

	t.Run("Get_NotFound", func(t *testing.T) {
		b := factory(t)
		_, err := b.Get(context.Background(), "does-not-exist")
		if !errors.Is(err, server.ErrJobNotFound) {
			t.Errorf("Get nonexistent: got %v, want ErrJobNotFound", err)
		}
	})

	t.Run("FullLifecycle", func(t *testing.T) {
		b := factory(t)
		ctx := context.Background()

		// Enqueue.
		j, err := b.Enqueue(ctx, server.EnqueueOptions{
			Kind:       server.JobBackup,
			Deployment: "db1",
			RepoURL:    "file:///srv/repo",
			Args:       map[string]any{"compression": "zstd", "level": float64(7)},
		})
		if err != nil {
			t.Fatal(err)
		}
		if j.State != server.JobQueued {
			t.Errorf("State = %q, want queued", j.State)
		}
		if j.ID == "" {
			t.Error("ID should be assigned by backend")
		}
		if j.RepoURL != "file:///srv/repo" {
			t.Errorf("RepoURL = %q", j.RepoURL)
		}

		// Get round-trip preserves Args.
		got, err := b.Get(ctx, j.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Args["compression"] != "zstd" {
			t.Errorf("Args round-trip lost compression: %+v", got.Args)
		}

		// Mismatched-deployment claim → ErrNoJobs.
		if _, err := b.Claim(ctx, server.ClaimOptions{
			AgentID:     "agent-1",
			Deployments: []string{"db2"},
		}); !errors.Is(err, server.ErrNoJobs) {
			t.Errorf("Claim mismatched deployment: got %v, want ErrNoJobs", err)
		}

		// Successful claim.
		claimed, err := b.Claim(ctx, server.ClaimOptions{
			AgentID:     "agent-1",
			Deployments: []string{"db1", "db2"},
			Kinds:       []server.JobKind{server.JobBackup},
		})
		if err != nil {
			t.Fatal(err)
		}
		if claimed.ID != j.ID {
			t.Errorf("Claim returned wrong job: got %q, want %q", claimed.ID, j.ID)
		}
		if claimed.State != server.JobRunning {
			t.Errorf("State = %q, want running", claimed.State)
		}
		if claimed.AssignedTo != "agent-1" {
			t.Errorf("AssignedTo = %q", claimed.AssignedTo)
		}
		if claimed.StartedAt == nil {
			t.Error("StartedAt should be set on claim")
		}

		// Re-claim → ErrNoJobs (the queue is now empty).
		if _, err := b.Claim(ctx, server.ClaimOptions{
			AgentID:     "agent-2",
			Deployments: []string{"db1"},
		}); !errors.Is(err, server.ErrNoJobs) {
			t.Errorf("re-claim: got %v, want ErrNoJobs", err)
		}

		// AppendProgress while running.
		if err := b.AppendProgress(ctx, j.ID, server.ProgressEvent{
			Op:   "backup.progress",
			Body: map[string]any{"bytes": float64(1024)},
		}); err != nil {
			t.Errorf("AppendProgress: %v", err)
		}

		// Verify progress recorded.
		mid, err := b.Get(ctx, j.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(mid.Progress) != 1 {
			t.Errorf("Progress len = %d, want 1", len(mid.Progress))
		}
		if mid.Progress[0].Op != "backup.progress" {
			t.Errorf("Progress[0].Op = %q", mid.Progress[0].Op)
		}

		// Complete with success.
		done, err := b.Complete(ctx, j.ID, server.CompleteOptions{
			Success: true,
			Result:  map[string]any{"backup_id": "db1.full.x"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if done.State != server.JobCompleted {
			t.Errorf("State = %q, want completed", done.State)
		}
		if done.Result["backup_id"] != "db1.full.x" {
			t.Errorf("Result not stored: %+v", done.Result)
		}
		if done.CompletedAt == nil {
			t.Error("CompletedAt should be set")
		}

		// Idempotent re-completion: same state, no error.
		done2, err := b.Complete(ctx, j.ID, server.CompleteOptions{Success: false, Failure: "should be ignored"})
		if err != nil {
			t.Errorf("re-complete: %v", err)
		}
		if done2.State != server.JobCompleted {
			t.Errorf("re-complete state = %q, want still completed", done2.State)
		}
		if done2.Failure != "" {
			t.Errorf("re-complete must not overwrite Failure: got %q", done2.Failure)
		}

		// Progress on a terminal job → ErrJobNotRunning.
		if err := b.AppendProgress(ctx, j.ID, server.ProgressEvent{Op: "x"}); !errors.Is(err, server.ErrJobNotRunning) {
			t.Errorf("AppendProgress after complete: got %v, want ErrJobNotRunning", err)
		}
	})

	t.Run("FIFOOrder", func(t *testing.T) {
		b := factory(t)
		ctx := context.Background()

		// Enqueue three jobs with explicit (visible) ordering. We sleep
		// 1ms between Enqueues so created_at is monotonically distinct
		// even on the fastest hardware.
		var ids []string
		for i := 0; i < 3; i++ {
			j, err := b.Enqueue(ctx, server.EnqueueOptions{
				Kind:       server.JobBackup,
				Deployment: "db1",
			})
			if err != nil {
				t.Fatal(err)
			}
			ids = append(ids, j.ID)
			time.Sleep(2 * time.Millisecond)
		}

		// Claim once → must yield the oldest (ids[0]).
		first, err := b.Claim(ctx, server.ClaimOptions{
			AgentID:     "a",
			Deployments: []string{"db1"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if first.ID != ids[0] {
			t.Errorf("first claim = %q, want oldest %q", first.ID, ids[0])
		}

		// Claim again → next oldest.
		second, err := b.Claim(ctx, server.ClaimOptions{
			AgentID:     "a",
			Deployments: []string{"db1"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if second.ID != ids[1] {
			t.Errorf("second claim = %q, want %q", second.ID, ids[1])
		}
	})

	t.Run("Filter_DeploymentAndKind", func(t *testing.T) {
		b := factory(t)
		ctx := context.Background()

		_, err := b.Enqueue(ctx, server.EnqueueOptions{Kind: server.JobBackup, Deployment: "db1"})
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
		_, err = b.Enqueue(ctx, server.EnqueueOptions{Kind: server.JobVerify, Deployment: "db1"})
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
		_, err = b.Enqueue(ctx, server.EnqueueOptions{Kind: server.JobBackup, Deployment: "db2"})
		if err != nil {
			t.Fatal(err)
		}

		// Agent only handles backups, only db1 → must skip the verify
		// job (newer, but wrong kind) and the db2 job (wrong deployment)
		// and pick the db1/backup.
		got, err := b.Claim(ctx, server.ClaimOptions{
			AgentID:     "a",
			Deployments: []string{"db1"},
			Kinds:       []server.JobKind{server.JobBackup},
		})
		if err != nil {
			t.Fatal(err)
		}
		if got.Kind != server.JobBackup || got.Deployment != "db1" {
			t.Errorf("filter mismatch: got %+v", got)
		}
	})

	t.Run("Cancel", func(t *testing.T) {
		b := factory(t)
		ctx := context.Background()
		j, err := b.Enqueue(ctx, server.EnqueueOptions{Kind: server.JobBackup, Deployment: "db1"})
		if err != nil {
			t.Fatal(err)
		}
		cancelled, err := b.Cancel(ctx, j.ID, "operator request")
		if err != nil {
			t.Fatal(err)
		}
		if cancelled.State != server.JobCancelled {
			t.Errorf("State = %q, want cancelled", cancelled.State)
		}
		if cancelled.Failure != "cancelled: operator request" {
			t.Errorf("Failure = %q", cancelled.Failure)
		}

		// Re-cancel is a no-op.
		again, err := b.Cancel(ctx, j.ID, "double-cancel")
		if err != nil {
			t.Errorf("re-cancel: %v", err)
		}
		if again.Failure != "cancelled: operator request" {
			t.Errorf("re-cancel must not overwrite Failure: got %q", again.Failure)
		}
	})

	t.Run("List_FiltersAndOrdersNewestFirst", func(t *testing.T) {
		b := factory(t)
		ctx := context.Background()

		_, err := b.Enqueue(ctx, server.EnqueueOptions{Kind: server.JobBackup, Deployment: "db1"})
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
		latest, err := b.Enqueue(ctx, server.EnqueueOptions{Kind: server.JobBackup, Deployment: "db1"})
		if err != nil {
			t.Fatal(err)
		}

		// List all. Newest should come first.
		all, err := b.List(ctx, server.ListOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(all) != 2 {
			t.Errorf("len = %d, want 2", len(all))
		}
		if all[0].ID != latest.ID {
			t.Errorf("ordering: got %q first, want newest %q first", all[0].ID, latest.ID)
		}

		// Limit honoured.
		one, err := b.List(ctx, server.ListOptions{Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		if len(one) != 1 {
			t.Errorf("Limit=1 returned %d", len(one))
		}

		// State filter.
		_, err = b.Claim(ctx, server.ClaimOptions{AgentID: "a", Deployments: []string{"db1"}})
		if err != nil {
			t.Fatal(err)
		}
		queued, err := b.List(ctx, server.ListOptions{State: server.JobQueued})
		if err != nil {
			t.Fatal(err)
		}
		if len(queued) != 1 {
			t.Errorf("queued count = %d, want 1", len(queued))
		}
	})

	t.Run("SweepAbandoned", func(t *testing.T) {
		b := factory(t)
		ctx := context.Background()

		j, err := b.Enqueue(ctx, server.EnqueueOptions{Kind: server.JobBackup, Deployment: "db1"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := b.Claim(ctx, server.ClaimOptions{
			AgentID:     "agent-x",
			Deployments: []string{"db1"},
		}); err != nil {
			t.Fatal(err)
		}

		// Deadline of 0 means "anything currently running is past
		// deadline" → must reap.
		n, err := b.SweepAbandoned(ctx, 0)
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("reaped = %d, want 1", n)
		}
		got, err := b.Get(ctx, j.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != server.JobFailed {
			t.Errorf("State = %q, want failed", got.State)
		}
		if got.Failure == "" {
			t.Error("Failure should be set with abandoned reason")
		}

		// Second sweep is a no-op.
		n2, err := b.SweepAbandoned(ctx, 0)
		if err != nil {
			t.Fatal(err)
		}
		if n2 != 0 {
			t.Errorf("second sweep reaped %d, want 0", n2)
		}
	})

	t.Run("Cancel_NotFound", func(t *testing.T) {
		b := factory(t)
		_, err := b.Cancel(context.Background(), "nonexistent", "x")
		if !errors.Is(err, server.ErrJobNotFound) {
			t.Errorf("Cancel nonexistent: got %v, want ErrJobNotFound", err)
		}
	})

	t.Run("Complete_NotFound", func(t *testing.T) {
		b := factory(t)
		_, err := b.Complete(context.Background(), "nonexistent", server.CompleteOptions{Success: true})
		if !errors.Is(err, server.ErrJobNotFound) {
			t.Errorf("Complete nonexistent: got %v, want ErrJobNotFound", err)
		}
	})

	t.Run("AppendProgress_NotFound", func(t *testing.T) {
		b := factory(t)
		err := b.AppendProgress(context.Background(), "nonexistent", server.ProgressEvent{Op: "x"})
		if !errors.Is(err, server.ErrJobNotFound) {
			t.Errorf("AppendProgress nonexistent: got %v, want ErrJobNotFound", err)
		}
	})
}

// TestMemoryBackend_Contract runs the shared backend contract against
// the in-memory backend. This is the always-on side of the contract;
// the PG side runs in jobs_pg_integration_test.go behind the
// `integration` build tag.
func TestMemoryBackend_Contract(t *testing.T) {
	runBackendContract(t, func(t *testing.T) server.JobBackend {
		t.Helper()
		b := server.NewMemoryBackend()
		t.Cleanup(func() { _ = b.Close() })
		return b
	})
}
