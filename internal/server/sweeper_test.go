package server_test

import (
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/server"
)

// TestSweeper_ReclaimAbandoned verifies that a Running job whose
// StartedAt is older than the registry's claim deadline transitions
// to JobFailed with a structured "abandoned" message.
//
// Construction: enqueue → claim (transitions to Running with
// StartedAt=now) → set claim deadline to 1ns so anything Running is
// already past it → SweepAbandoned should reap it.
func TestSweeper_ReclaimAbandoned(t *testing.T) {
	r := server.NewJobRegistry().WithClaimDeadline(time.Nanosecond)

	job, err := r.Enqueue(server.EnqueueOptions{
		Kind:       server.JobBackup,
		Deployment: "db1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Claim(server.ClaimOptions{
		AgentID:     "a",
		Deployments: []string{"db1"},
	}); err != nil {
		t.Fatal(err)
	}
	// Sleep to ensure StartedAt is at least 1ns in the past.
	time.Sleep(time.Millisecond)

	reaped := r.SweepAbandoned()
	if reaped != 1 {
		t.Errorf("reaped = %d, want 1", reaped)
	}
	got, err := r.Get(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != server.JobFailed {
		t.Errorf("State = %q, want failed", got.State)
	}
	if !strings.Contains(got.Failure, "abandoned") {
		t.Errorf("Failure = %q; want substring 'abandoned'", got.Failure)
	}
}

// TestSweeper_LeavesActiveAlone — a Running job within the deadline
// is not reaped.
func TestSweeper_LeavesActiveAlone(t *testing.T) {
	r := server.NewJobRegistry().WithClaimDeadline(time.Hour)

	if _, err := r.Enqueue(server.EnqueueOptions{
		Kind:       server.JobBackup,
		Deployment: "db1",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Claim(server.ClaimOptions{
		AgentID:     "a",
		Deployments: []string{"db1"},
	}); err != nil {
		t.Fatal(err)
	}
	if reaped := r.SweepAbandoned(); reaped != 0 {
		t.Errorf("reaped = %d, want 0 (deadline is 1h, job is fresh)", reaped)
	}
}

// TestSweeper_LeavesQueuedAlone — Queued jobs aren't reaped (they're
// not running yet, so "abandoned" doesn't apply).
func TestSweeper_LeavesQueuedAlone(t *testing.T) {
	r := server.NewJobRegistry().WithClaimDeadline(time.Nanosecond)
	if _, err := r.Enqueue(server.EnqueueOptions{
		Kind:       server.JobBackup,
		Deployment: "db1",
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	if reaped := r.SweepAbandoned(); reaped != 0 {
		t.Errorf("reaped = %d, want 0 (queued jobs are not abandoned)", reaped)
	}
}

// TestSweeper_FreshProgressSurvivesThenReapedOnSilence pins the fix:
// abandonment keys on UpdatedAt (last activity), NOT StartedAt. A job
// whose StartedAt is well past the deadline must SURVIVE as long as it
// keeps reporting progress (a long-but-healthy backup), and only be
// reaped once it falls silent past the deadline.
func TestSweeper_FreshProgressSurvivesThenReapedOnSilence(t *testing.T) {
	const deadline = 40 * time.Millisecond
	r := server.NewJobRegistry().WithClaimDeadline(deadline)

	job, err := r.Enqueue(server.EnqueueOptions{Kind: server.JobBackup, Deployment: "db1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Claim(server.ClaimOptions{AgentID: "a", Deployments: []string{"db1"}}); err != nil {
		t.Fatal(err)
	}

	// Age StartedAt well beyond the deadline — under the old StartedAt
	// rule this alone would condemn the job.
	time.Sleep(2 * deadline)

	// Agent is alive and reporting: refresh UpdatedAt, then sweep. No
	// sleep between progress and sweep, so UpdatedAt is comfortably
	// inside the deadline regardless of scheduler jitter.
	if err := r.AppendProgress(job.ID, server.ProgressEvent{At: time.Now().UTC(), Op: "agent.progress"}); err != nil {
		t.Fatal(err)
	}
	if reaped := r.SweepAbandoned(); reaped != 0 {
		t.Fatalf("healthy job that just reported progress was reaped = %d, want 0 (StartedAt is old but UpdatedAt is fresh)", reaped)
	}
	if got, _ := r.Get(job.ID); got.State != server.JobRunning {
		t.Fatalf("State = %q, want running (must not be abandoned while reporting)", got.State)
	}

	// Now the agent goes silent past the deadline → genuinely abandoned.
	time.Sleep(2 * deadline)
	if reaped := r.SweepAbandoned(); reaped != 1 {
		t.Fatalf("silent job past deadline reaped = %d, want 1", reaped)
	}
	got, _ := r.Get(job.ID)
	if got.State != server.JobFailed || !strings.Contains(got.Failure, "abandoned") {
		t.Errorf("State=%q Failure=%q; want failed + 'abandoned'", got.State, got.Failure)
	}
}
