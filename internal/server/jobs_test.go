package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/server"
)

func TestJobRegistry_Lifecycle(t *testing.T) {
	r := server.NewJobRegistry()

	// Enqueue.
	j, err := r.Enqueue(server.EnqueueOptions{
		Kind:       server.JobBackup,
		Deployment: "db1",
		RepoURL:    "file:///srv/repo",
	})
	if err != nil {
		t.Fatal(err)
	}
	if j.State != server.JobQueued {
		t.Errorf("State = %q, want queued", j.State)
	}

	// Get round-trips.
	got, err := r.Get(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != j.ID || got.Deployment != "db1" {
		t.Errorf("Get returned %+v", got)
	}

	// Claim by an agent that doesn't manage db1 → no-jobs.
	if _, err := r.Claim(server.ClaimOptions{
		AgentID:     "agent-1",
		Deployments: []string{"db2"},
	}); !errors.Is(err, server.ErrNoJobs) {
		t.Errorf("expected ErrNoJobs for non-matching deployments; got %v", err)
	}

	// Claim by an agent that DOES → success, state goes Running.
	claimed, err := r.Claim(server.ClaimOptions{
		AgentID:     "agent-1",
		Deployments: []string{"db1", "db2"},
	})
	if err != nil {
		t.Fatal(err)
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

	// Re-claim (same FIFO bucket exhausted) → no-jobs.
	if _, err := r.Claim(server.ClaimOptions{
		AgentID:     "agent-2",
		Deployments: []string{"db1"},
	}); !errors.Is(err, server.ErrNoJobs) {
		t.Errorf("expected ErrNoJobs; got %v", err)
	}

	// Append progress while Running.
	if err := r.AppendProgress(j.ID, server.ProgressEvent{
		Op:   "backup.progress",
		Body: map[string]any{"bytes_logical": 1024},
	}); err != nil {
		t.Errorf("AppendProgress: %v", err)
	}

	// Complete with success.
	done, err := r.Complete(j.ID, server.CompleteOptions{
		Success: true,
		Result:  map[string]any{"backup_id": "db1.full.x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if done.State != server.JobCompleted {
		t.Errorf("State = %q, want completed", done.State)
	}

	// Re-completion is idempotent (no error).
	if _, err := r.Complete(j.ID, server.CompleteOptions{Success: true}); err != nil {
		t.Errorf("re-complete should be idempotent; got %v", err)
	}

	// AppendProgress after complete returns ErrJobNotRunning.
	if err := r.AppendProgress(j.ID, server.ProgressEvent{Op: "x"}); !errors.Is(err, server.ErrJobNotRunning) {
		t.Errorf("expected ErrJobNotRunning; got %v", err)
	}
}

func TestJobRegistry_FIFOOrder(t *testing.T) {
	r := server.NewJobRegistry()
	for i := 0; i < 3; i++ {
		_, err := r.Enqueue(server.EnqueueOptions{
			Kind:       server.JobBackup,
			Deployment: "db1",
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// First claim takes the oldest.
	_, err := r.Claim(server.ClaimOptions{AgentID: "a", Deployments: []string{"db1"}})
	if err != nil {
		t.Fatal(err)
	}
	// Second claim takes the next-oldest.
	_, err = r.Claim(server.ClaimOptions{AgentID: "a", Deployments: []string{"db1"}})
	if err != nil {
		t.Fatal(err)
	}
	listed := r.List(server.ListOptions{State: server.JobRunning})
	if len(listed) != 2 {
		t.Errorf("want 2 running; got %d", len(listed))
	}
}

// End-to-end HTTP test: enqueue → list → claim → progress → complete.
func TestEndToEnd_DispatchOverHTTP(t *testing.T) {
	s, err := server.New(server.Config{Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(s.Handler())
	defer hs.Close()

	// 1. Enqueue.
	resp, err := http.Post(hs.URL+"/v1/deployments/db1/backups",
		"application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("enqueue: status=%d body=%s", resp.StatusCode, body)
	}
	var enqEnv struct {
		Result struct {
			ID string `json:"id"`
		} `json:"result"`
	}
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &enqEnv); err != nil {
		t.Fatalf("decode enqueue: %v\n%s", err, body)
	}
	jobID := enqEnv.Result.ID
	if jobID == "" {
		t.Fatalf("enqueue returned no job ID: %s", body)
	}

	// 2. Claim.
	claimBody := `{"agent_id":"agent-1","deployments":["db1"]}`
	resp2, err := http.Post(hs.URL+"/v1/jobs/claim", "application/json", strings.NewReader(claimBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp2.Body)
		t.Fatalf("claim: status=%d body=%s", resp2.StatusCode, body)
	}

	// 3. Progress.
	progBody := `{"op":"backup.progress","body":{"bytes":1024}}`
	resp3, err := http.Post(hs.URL+"/v1/jobs/"+jobID+"/progress",
		"application/json", strings.NewReader(progBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp3.Body)
		t.Fatalf("progress: status=%d body=%s", resp3.StatusCode, body)
	}

	// 4. Complete.
	completeBody := `{"success":true,"result":{"backup_id":"db1.full.x"}}`
	resp4, err := http.Post(hs.URL+"/v1/jobs/"+jobID+"/complete",
		"application/json", strings.NewReader(completeBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp4.Body.Close()
	if resp4.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp4.Body)
		t.Fatalf("complete: status=%d body=%s", resp4.StatusCode, body)
	}

	// 5. Final GET — state should be completed, with the progress
	// event recorded.
	resp5, err := http.Get(hs.URL + "/v1/jobs/" + jobID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp5.Body.Close()
	finalBody, _ := io.ReadAll(resp5.Body)
	if !strings.Contains(string(finalBody), `"state": "completed"`) {
		t.Errorf("final state not completed: %s", finalBody)
	}
	if !strings.Contains(string(finalBody), `"backup_id": "db1.full.x"`) {
		t.Errorf("result not recorded: %s", finalBody)
	}
	if !strings.Contains(string(finalBody), `"backup.progress"`) {
		t.Errorf("progress event not recorded: %s", finalBody)
	}
}

// TestClaim_NoEligibleJobs surfaces the structured 404 + code so a
// polling agent can distinguish "no work" from a real error.
func TestClaim_NoEligibleJobs(t *testing.T) {
	s, err := server.New(server.Config{Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(s.Handler())
	defer hs.Close()

	resp, err := http.Post(hs.URL+"/v1/jobs/claim", "application/json",
		strings.NewReader(`{"agent_id":"a"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "notfound.no_jobs") {
		t.Errorf("expected notfound.no_jobs code; got %s", body)
	}
}

// silence-unused for the imports we share with server_test.go.
var _ = context.Background
