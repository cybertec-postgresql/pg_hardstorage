package server_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/server"
)

// newTestServer builds a *server.Server pointed at a fresh in-memory
// repo for the test. Returns the server + an httptest server that
// proxies requests through; the caller closes both.
func newTestServer(t *testing.T) (*server.Server, *httptest.Server) {
	t.Helper()
	s, err := server.New(server.Config{
		Listen:           "127.0.0.1:0",
		HeartbeatTimeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	// We don't call s.Run — that binds a real listener. Instead we
	// drive the routes via httptest which exercises the same mux.
	hs := httptest.NewServer(testHandler(s))
	t.Cleanup(hs.Close)
	return s, hs
}

// testHandler exposes the server's internal mux. server.Server
// doesn't export it; we work around by delegating to s.Run via a
// background goroutine on a real listener instead. Cleaner: extract
// the mux into a test helper exported from server_test.go.
//
// For these tests, we use a separate exported Routes() helper.
func testHandler(s *server.Server) http.Handler {
	return s.Handler()
}

func TestHandleHealthz_NoAuth(t *testing.T) {
	_, hs := newTestServer(t)

	resp, err := http.Get(hs.URL + "/v1/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var env map[string]any
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	if env["schema"] != "pg_hardstorage.server.v1" {
		t.Errorf("schema = %v", env["schema"])
	}
}

func TestHandleAgents_HeartbeatRoundTrip(t *testing.T) {
	_, hs := newTestServer(t)

	body := `{"id":"agent-1","host":"db1.example.com","version":"v0.3.0","deployments":["db1","db2"]}`
	resp, err := http.Post(hs.URL+"/v1/agents/heartbeat", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		t.Fatalf("heartbeat status = %d: %s", resp.StatusCode, bodyBytes)
	}

	// Now list and assert the agent shows up.
	resp2, err := http.Get(hs.URL + "/v1/agents")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	bodyBytes, _ := io.ReadAll(resp2.Body)
	if !strings.Contains(string(bodyBytes), "agent-1") {
		t.Errorf("agent-1 not in /v1/agents response: %s", bodyBytes)
	}
}

func TestHandleAgents_RejectsBadHeartbeat(t *testing.T) {
	_, hs := newTestServer(t)

	resp, err := http.Post(hs.URL+"/v1/agents/heartbeat", "application/json",
		strings.NewReader(`{"host":"missing-id.example.com"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestRequireAuth_RejectsMissingToken(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := writeFile(tokenPath, "secret-token-1234567890\n"); err != nil {
		t.Fatal(err)
	}
	s, err := server.New(server.Config{
		Listen:           "127.0.0.1:0",
		HeartbeatTimeout: 30 * time.Second,
		Auth:             server.AuthConfig{TokenFile: tokenPath},
	})
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(s.Handler())
	defer hs.Close()

	resp, err := http.Get(hs.URL + "/v1/version")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 without token; got %d", resp.StatusCode)
	}
	// healthz remains open.
	resp2, err := http.Get(hs.URL + "/v1/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("healthz blocked by auth: %d", resp2.StatusCode)
	}
}

func TestRequireAuth_AcceptsValidToken(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := writeFile(tokenPath, "secret-token-1234567890\n"); err != nil {
		t.Fatal(err)
	}
	s, err := server.New(server.Config{
		Listen:           "127.0.0.1:0",
		HeartbeatTimeout: 30 * time.Second,
		Auth:             server.AuthConfig{TokenFile: tokenPath},
	})
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(s.Handler())
	defer hs.Close()

	req, _ := http.NewRequest(http.MethodGet, hs.URL+"/v1/version", nil)
	req.Header.Set("Authorization", "Bearer secret-token-1234567890")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 with valid token; got %d", resp.StatusCode)
	}
}

// TestRun_GracefulShutdown verifies the SIGINT-equivalent ctx cancel
// produces a clean exit and the listener actually unbinds. We use
// :0 so the OS picks a free port; if shutdown didn't actually stop
// the listener, the test would hang past its deadline.
func TestRun_GracefulShutdown(t *testing.T) {
	s, err := server.New(server.Config{Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	// Give Run a beat to bind.
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v on graceful shutdown; want nil", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return within 15s of cancel — graceful shutdown is broken")
	}
}

// writeFile helper.
func writeFile(path, body string) error {
	return osWriteFile(path, []byte(body))
}
