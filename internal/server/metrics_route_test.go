package server_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/server"
)

// get fetches a path and returns the body, failing the test on error.
func get(t *testing.T, base, path string) (int, string) {
	t.Helper()
	resp, err := http.Get(base + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestMetricsEndpointServesRealData stands up a control plane with a
// queued job, a registered agent, and two configured repos, then scrapes
// /metrics and asserts the scrape carries real, server-specific data —
// the regression this whole feature fixes ("we don't see data").
func TestMetricsEndpointServesRealData(t *testing.T) {
	s, err := server.New(server.Config{
		Listen: "127.0.0.1:0",
		Repos:  []string{"file:///tmp/repo-a", "file:///tmp/repo-b"},
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}

	// One queued backup job.
	if _, err := s.Jobs().Enqueue(server.EnqueueOptions{
		Kind:       server.JobBackup,
		Deployment: "db1",
		RepoURL:    "file:///tmp/repo-a",
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// One registered agent.
	if _, err := s.Agents().Heartbeat(server.HeartbeatRequest{ID: "agent-1", Host: "host-1"}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}

	hs := httptest.NewServer(s.Handler())
	defer hs.Close()

	// Make a prior request so the HTTP-request counter has something to
	// show (the middleware records a request AFTER its handler returns,
	// so the /metrics scrape never counts itself).
	if code, _ := get(t, hs.URL, "/v1/healthz"); code != http.StatusOK {
		t.Fatalf("healthz status = %d", code)
	}

	code, body := get(t, hs.URL, "/metrics")
	if code != http.StatusOK {
		t.Fatalf("/metrics status = %d", code)
	}

	// Gauges are sampled from THIS server at scrape time → exact values.
	wantExact := []string{
		`pg_hardstorage_jobs{state="queued"} 1`,
		`pg_hardstorage_jobs{state="running"} 0`, // seeded zero proves the state set is complete
		`pg_hardstorage_agents{state="total"} 1`,
		`pg_hardstorage_agents{state="active"} 1`,
		`pg_hardstorage_repos_configured 2`,
	}
	for _, w := range wantExact {
		if !strings.Contains(body, w) {
			t.Errorf("metrics missing exact line %q\n--- body ---\n%s", w, body)
		}
	}

	// Build info is always present with value 1.
	if !strings.Contains(body, "pg_hardstorage_build_info{") {
		t.Errorf("missing build_info\n%s", body)
	}

	// Counters/histograms are process-global (other tests share the
	// registry), so assert presence of the family + the labelled series
	// our healthz request produced, not exact counts.
	wantPresent := []string{
		"# TYPE pg_hardstorage_http_requests_total counter",
		`pg_hardstorage_http_requests_total{route="healthz",method="GET",code="200"}`,
		"# TYPE pg_hardstorage_http_request_duration_seconds histogram",
		"pg_hardstorage_http_request_duration_seconds_bucket{",
	}
	for _, w := range wantPresent {
		if !strings.Contains(body, w) {
			t.Errorf("metrics missing %q\n--- body ---\n%s", w, body)
		}
	}

	// Content type is the Prometheus exposition media type.
	resp, err := http.Get(hs.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Errorf("content-type = %q", ct)
	}
}

// TestMetricsEndpointNeedsNoAuth confirms /metrics is reachable without a
// bearer token even when the rest of the API requires one.
func TestMetricsEndpointNeedsNoAuth(t *testing.T) {
	// A token file makes /v1/* require auth; /metrics must stay open.
	dir := t.TempDir()
	tokenPath := dir + "/token"
	// Mode 0600 — the server refuses group/world-readable token files.
	if err := os.WriteFile(tokenPath, []byte("s3cr3t\n"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	s, err := server.New(server.Config{
		Listen: "127.0.0.1:0",
		Auth:   server.AuthConfig{TokenFile: tokenPath},
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	hs := httptest.NewServer(s.Handler())
	defer hs.Close()

	// /metrics: no Authorization header, still 200.
	if code, _ := get(t, hs.URL, "/metrics"); code != http.StatusOK {
		t.Errorf("/metrics without token: status = %d, want 200", code)
	}
	// /v1/version: no header → 401, proving auth is actually on.
	if code, _ := get(t, hs.URL, "/v1/version"); code != http.StatusUnauthorized {
		t.Errorf("/v1/version without token: status = %d, want 401", code)
	}
}
