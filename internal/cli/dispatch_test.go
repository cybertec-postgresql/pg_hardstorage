package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestDispatchClient_EnqueueRestore_HappyPath asserts the POST shape +
// the result-id extraction.
func TestDispatchClient_EnqueueRestore_HappyPath(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/deployments/db1/restores", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["backup_id"] != "latest" {
			t.Errorf("body.backup_id = %v", body["backup_id"])
		}
		if body["target_dir"] != "/tmp/restored" {
			t.Errorf("body.target_dir = %v", body["target_dir"])
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		io.WriteString(w, `{"result":{"id":"job-abc"}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &DispatchClient{BaseURL: srv.URL}
	id, err := c.EnqueueRestore(context.Background(), "db1", map[string]any{
		"backup_id":  "latest",
		"target_dir": "/tmp/restored",
	})
	if err != nil {
		t.Fatal(err)
	}
	if id != "job-abc" {
		t.Errorf("id = %q, want job-abc", id)
	}
}

// TestDispatchClient_EnqueueBearerToken asserts the bearer header.
func TestDispatchClient_EnqueueBearerToken(t *testing.T) {
	var gotAuth string
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/deployments/db1/restores", func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusAccepted)
		io.WriteString(w, `{"result":{"id":"x"}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &DispatchClient{BaseURL: srv.URL, Token: "shhh-secret"}
	if _, err := c.EnqueueRestore(context.Background(), "db1", map[string]any{
		"backup_id":  "x",
		"target_dir": "/tmp",
	}); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer shhh-secret" {
		t.Errorf("Authorization = %q", gotAuth)
	}
}

// TestDispatchClient_EnqueueServerError surfaces a structured error.
func TestDispatchClient_EnqueueServerError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/deployments/db1/restores", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"code":"usage.missing_field","message":"target_dir is required"}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &DispatchClient{BaseURL: srv.URL}
	_, err := c.EnqueueRestore(context.Background(), "db1", map[string]any{
		"backup_id": "x",
	})
	if err == nil {
		t.Fatal("expected error on 400")
	}
	if !strings.Contains(err.Error(), "status=400") {
		t.Errorf("error should include status code: %v", err)
	}
}

// TestDispatchClient_PollUntilTerminal exercises the poll loop end to end:
// the server returns three states (running with progress, running with more
// progress, completed) and the client should forward only new events
// and stop on completed.
func TestDispatchClient_PollUntilTerminal(t *testing.T) {
	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/jobs/job-1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		i := hits.Add(1)
		switch i {
		case 1:
			io.WriteString(w, `{"result":{"id":"job-1","kind":"restore","deployment":"db1","state":"running","progress":[{"at":"2026-04-29T00:00:00Z","op":"backup.progress","body":{"step":1}}]}}`)
		case 2:
			io.WriteString(w, `{"result":{"id":"job-1","kind":"restore","deployment":"db1","state":"running","progress":[{"at":"2026-04-29T00:00:00Z","op":"backup.progress","body":{"step":1}},{"at":"2026-04-29T00:00:01Z","op":"backup.progress","body":{"step":2}}]}}`)
		default:
			io.WriteString(w, `{"result":{"id":"job-1","kind":"restore","deployment":"db1","state":"completed","result":{"backup_id":"db1.full.x"},"progress":[{"at":"2026-04-29T00:00:00Z","op":"backup.progress","body":{"step":1}},{"at":"2026-04-29T00:00:01Z","op":"backup.progress","body":{"step":2}}]}}`)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &DispatchClient{BaseURL: srv.URL, PollInterval: 5 * time.Millisecond}

	var seen []ProgressEvt
	job, err := c.PollUntilTerminal(context.Background(), "job-1", func(ev ProgressEvt) {
		seen = append(seen, ev)
	})
	if err != nil {
		t.Fatal(err)
	}
	if job.State != "completed" {
		t.Errorf("State = %q, want completed", job.State)
	}
	if len(seen) != 2 {
		t.Errorf("forwarded %d events, want 2 (no duplicates across polls)", len(seen))
	}
	if seen[0].Op != "backup.progress" || seen[0].Body["step"] != float64(1) {
		t.Errorf("first event wrong: %+v", seen[0])
	}
	if seen[1].Body["step"] != float64(2) {
		t.Errorf("second event wrong: %+v", seen[1])
	}
}

// TestDispatchClient_PollUntilTerminal_FailedReturns surfaces the
// failed terminal state cleanly. The client doesn't error on a
// "failed" job — that's the caller's job. PollUntilTerminal returns
// the job; the caller maps state→exit-code.
func TestDispatchClient_PollUntilTerminal_FailedReturns(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/jobs/job-bad", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"result":{"id":"job-bad","state":"failed","failure":"chunker oom"}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &DispatchClient{BaseURL: srv.URL, PollInterval: 5 * time.Millisecond}
	job, err := c.PollUntilTerminal(context.Background(), "job-bad", nil)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != "failed" {
		t.Errorf("State = %q, want failed", job.State)
	}
	if job.Failure != "chunker oom" {
		t.Errorf("Failure = %q", job.Failure)
	}
}

// TestDispatchClient_PollHonoursContextCancel asserts ctx-cancel
// breaks the poll loop promptly.
func TestDispatchClient_PollHonoursContextCancel(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/jobs/job-running", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"result":{"id":"job-running","state":"running","progress":[]}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &DispatchClient{BaseURL: srv.URL, PollInterval: 50 * time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(75 * time.Millisecond)
		cancel()
	}()

	t0 := time.Now()
	_, err := c.PollUntilTerminal(ctx, "job-running", nil)
	elapsed := time.Since(t0)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if elapsed > time.Second {
		t.Errorf("poll took %v after cancel — should bail much faster", elapsed)
	}
}

// TestDispatchClient_RequiresBaseURL surfaces a clear error when
// callers forget to set BaseURL.
func TestDispatchClient_RequiresBaseURL(t *testing.T) {
	c := &DispatchClient{}
	_, err := c.EnqueueRestore(context.Background(), "db1", map[string]any{
		"backup_id":  "x",
		"target_dir": "/tmp",
	})
	if err == nil {
		t.Fatal("expected error when BaseURL is empty")
	}
	if !strings.Contains(err.Error(), "BaseURL") {
		t.Errorf("error should mention BaseURL: %v", err)
	}
}
