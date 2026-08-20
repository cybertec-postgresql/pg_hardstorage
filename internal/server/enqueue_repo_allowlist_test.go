package server_test

// SEC-2 defence-in-depth: the control plane must not dispatch a
// job to a repo it does not know. An explicit `repo` in the request
// body that is not in server.repos is rejected with 400 (the agent's
// per-deployment guard is the last line of defence, but a
// misconfigured or hostile control plane should not even be able to
// queue such a job). A repo in the configured list — including a
// trailing-slash spelling — and the implicit default (repos[0])
// keep working. A control plane configured with no repos skips the
// check: it has no allowlist to enforce, and the agent guard still
// applies at claim time.

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/server"
)

func postStatus(ts *httptest.Server, t *testing.T, path, body string) int {
	t.Helper()
	resp, err := http.Post(ts.URL+path, "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func TestEnqueue_RepoAllowlist(t *testing.T) {
	s, err := server.New(server.Config{
		Listen: "127.0.0.1:0",
		Repos:  []string{"file:///srv/allowed-a"},
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	cases := []struct {
		path, body string
		want       int
		what       string
	}{
		{"/v1/deployments/db1/backups", `{"repo":"file:///srv/allowed-a"}`, http.StatusAccepted, "configured repo"},
		{"/v1/deployments/db1/backups", `{"repo":"file:///srv/allowed-a/"}`, http.StatusAccepted, "trailing-slash spelling"},
		{"/v1/deployments/db1/backups", `{"repo":"file:///srv/attacker"}`, http.StatusBadRequest, "off-list repo"},
		{"/v1/deployments/db1/backups", `{}`, http.StatusAccepted, "implicit default (repos[0])"},
		{"/v1/deployments/db1/verifies", `{"backup_id":"latest","repo":"file:///srv/attacker"}`, http.StatusBadRequest, "verify off-list repo"},
		{"/v1/deployments/db1/restores", `{"backup_id":"latest","target_dir":"/tmp/restore-sec2","repo":"file:///srv/attacker"}`, http.StatusBadRequest, "restore off-list repo"},
		{"/v1/deployments/db1/restores", `{"backup_id":"latest","target_dir":"/tmp/restore-sec2","repo":"file:///srv/allowed-a"}`, http.StatusAccepted, "restore configured repo"},
	}
	for _, c := range cases {
		if got := postStatus(ts, t, c.path, c.body); got != c.want {
			t.Errorf("%s: POST %s %s: got %d, want %d", c.what, c.path, c.body, got, c.want)
		}
	}
}

func TestEnqueue_RepoAllowlistSkippedWithoutConfiguredRepos(t *testing.T) {
	s, err := server.New(server.Config{Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	// No server.repos: no allowlist to enforce. The agent's
	// per-deployment repo guard remains the authority at claim time.
	if got := postStatus(ts, t, "/v1/deployments/db1/backups", `{"repo":"file:///srv/anywhere"}`); got != http.StatusAccepted {
		t.Errorf("no-repos control plane: got %d, want 202 (check skipped)", got)
	}
}
