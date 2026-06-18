package agent_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/agent"
)

// TestClient_HeartbeatsAndClaims drives the polling client against
// an httptest fixture that:
//   - records every heartbeat request,
//   - returns one job on the first claim then 404/no_jobs forever,
//   - records progress + complete posts.
//
// Asserts: at least one heartbeat fires; the claim returns the job;
// the executor runs and a complete post lands.
func TestClient_HeartbeatsAndClaims(t *testing.T) {
	var heartbeats, claims, progresses, completes atomic.Int32
	var jobIssued atomic.Bool

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/agents/heartbeat", func(w http.ResponseWriter, _ *http.Request) {
		heartbeats.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"result":{}}`))
	})
	mux.HandleFunc("/v1/jobs/claim", func(w http.ResponseWriter, _ *http.Request) {
		claims.Add(1)
		if jobIssued.Swap(true) {
			// Subsequent claims return no-jobs.
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"notfound.no_jobs","message":"no eligible jobs"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"result":{"id":"job-1","kind":"backup","deployment":"db1"}}`))
	})
	mux.HandleFunc("/v1/jobs/job-1/progress", func(w http.ResponseWriter, _ *http.Request) {
		progresses.Add(1)
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("/v1/jobs/job-1/complete", func(w http.ResponseWriter, _ *http.Request) {
		completes.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"result":{}}`))
	})
	hs := httptest.NewServer(mux)
	defer hs.Close()

	c := &agent.ControlPlaneClient{
		BaseURL:           hs.URL,
		AgentID:           "test-agent",
		Host:              "test-host",
		Version:           "test",
		Deployments:       []string{"db1"},
		HeartbeatInterval: 50 * time.Millisecond,
		PollInterval:      30 * time.Millisecond,
		JobExecutor:       &echoExecutor{},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	_ = c.Run(ctx)

	if heartbeats.Load() == 0 {
		t.Errorf("no heartbeats fired")
	}
	if claims.Load() == 0 {
		t.Errorf("no claims fired")
	}
	if progresses.Load() == 0 {
		t.Errorf("no progress events fired")
	}
	if completes.Load() != 1 {
		t.Errorf("completes = %d, want 1", completes.Load())
	}
}

// echoExecutor is a JobExecutor that emits one progress event and
// returns success.
type echoExecutor struct{}

func (echoExecutor) Execute(_ context.Context, j *agent.ControlPlaneJob, progress func(map[string]any)) (map[string]any, error) {
	progress(map[string]any{"echo": j.ID})
	return map[string]any{"executed": j.ID}, nil
}

// TestClient_CompleteRetriesOnTransientFailure: audit fix.
// The /complete POST is no longer best-effort; it retries with
// exponential backoff up to completeMaxAttempts.  We simulate two
// transient 500s followed by a 200 — the agent must persist
// through the retries and the control plane sees exactly one
// recorded completion.
func TestClient_CompleteRetriesOnTransientFailure(t *testing.T) {
	var heartbeats, claims, completeAttempts, completeOK atomic.Int32
	var jobIssued atomic.Bool

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/agents/heartbeat", func(w http.ResponseWriter, _ *http.Request) {
		heartbeats.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"result":{}}`))
	})
	mux.HandleFunc("/v1/jobs/claim", func(w http.ResponseWriter, _ *http.Request) {
		claims.Add(1)
		if jobIssued.Swap(true) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"notfound.no_jobs"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"result":{"id":"job-flaky","kind":"backup","deployment":"db1"}}`))
	})
	mux.HandleFunc("/v1/jobs/job-flaky/progress", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("/v1/jobs/job-flaky/complete", func(w http.ResponseWriter, _ *http.Request) {
		// First two attempts fail with 500; third succeeds.
		n := completeAttempts.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"code":"internal","message":"flake"}}`))
			return
		}
		completeOK.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"result":{}}`))
	})
	hs := httptest.NewServer(mux)
	defer hs.Close()

	c := &agent.ControlPlaneClient{
		BaseURL:           hs.URL,
		AgentID:           "test-agent",
		Host:              "test-host",
		Version:           "test",
		Deployments:       []string{"db1"},
		HeartbeatInterval: 50 * time.Millisecond,
		PollInterval:      30 * time.Millisecond,
		JobExecutor:       &echoExecutor{},
	}

	// 5s budget: enough for 500ms + 1s + 2s backoffs (3 attempts of
	// the worst case) plus the executor's tiny work.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.Run(ctx)

	if completeAttempts.Load() < 3 {
		t.Errorf("completeAttempts = %d, want >= 3 (retry should persist past 2 transient 500s)", completeAttempts.Load())
	}
	if completeOK.Load() != 1 {
		t.Errorf("completeOK = %d, want 1 (control plane should record exactly one successful completion)", completeOK.Load())
	}
}

// TestClient_CompleteGivesUpAfterBudget: a control plane that never
// returns 200 eventually exhausts the per-job retry budget and the
// agent surfaces the failure to stderr.  The Run loop itself
// continues (heartbeats + polls) — the give-up is per-job, not
// per-process — so we let the retry settle, cancel the context,
// then assert at least 2 attempts were made (i.e. retry actually
// fired).
func TestClient_CompleteGivesUpAfterBudget(t *testing.T) {
	var completeAttempts atomic.Int32
	var jobIssued atomic.Bool

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/agents/heartbeat", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"result":{}}`))
	})
	mux.HandleFunc("/v1/jobs/claim", func(w http.ResponseWriter, _ *http.Request) {
		if jobIssued.Swap(true) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"notfound.no_jobs"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"result":{"id":"job-broken","kind":"backup","deployment":"db1"}}`))
	})
	mux.HandleFunc("/v1/jobs/job-broken/progress", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("/v1/jobs/job-broken/complete", func(w http.ResponseWriter, _ *http.Request) {
		completeAttempts.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"internal"}}`))
	})
	hs := httptest.NewServer(mux)
	defer hs.Close()

	c := &agent.ControlPlaneClient{
		BaseURL:           hs.URL,
		AgentID:           "test-agent",
		Host:              "test-host",
		Version:           "test",
		Deployments:       []string{"db1"},
		HeartbeatInterval: 50 * time.Millisecond,
		PollInterval:      30 * time.Millisecond,
		JobExecutor:       &echoExecutor{},
	}

	// Generous overall timeout — the retry budget is internal but
	// known to be < 60s.  The Run loop continues polling forever
	// after the per-job retry exhausts; we cancel after the budget
	// has clearly elapsed.
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	_ = c.Run(ctx)

	if completeAttempts.Load() < 2 {
		t.Errorf("completeAttempts = %d, want >= 2 (retry should fire at least once on transient 500s)", completeAttempts.Load())
	}
}

// TestClient_RejectsMissingFields fails fast on misconfiguration.
func TestClient_RejectsMissingFields(t *testing.T) {
	cases := []agent.ControlPlaneClient{
		{},
		{BaseURL: "http://x"},
	}
	for _, cli := range cases {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		err := cli.Run(ctx)
		cancel()
		if err == nil {
			t.Errorf("Run(%+v) — expected error", cli)
		}
	}
}
