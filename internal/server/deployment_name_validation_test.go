package server_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/server"
)

// F-0004: the REST boundary must reject deployment names and backup IDs
// that would break out of, or splinter, the storage-key hierarchy
// (manifests/<dep>/backups/<id>/...). The manifest-store chokepoint
// (validateStorageID) already blocks traversal at read time, but the
// API surface used to answer a traversal name with a silent 200 + empty
// list instead of a clean 4xx — and enqueued jobs with hostile IDs only
// failed deep inside the agent. backup.ValidateDeployment /
// ValidateBackupID exist precisely for this boundary (manifest_store.go
// doc: "return a clean 4xx rather than relying solely on the store
// chokepoint") and had zero production callers.
//
// These tests pin the boundary contract:
//   - traversal deployment names → 400 before any storage access
//   - traversal backup IDs → 400 at enqueue
//   - legitimate names / the "latest" sentinel / a valid backup_id keep
//     working (no behavior change for good input).
func TestF0004_DeploymentNameAndBackupIDValidation(t *testing.T) {
	repoDir := t.TempDir()
	s, err := server.New(server.Config{
		Listen: "127.0.0.1:0",
		Repos:  []string{"file://" + repoDir},
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	get := func(path string) int {
		resp, err := ts.Client().Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	post := func(path, body string) int {
		resp, err := http.Post(ts.URL+path, "application/json", bytes.NewBufferString(body))
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	t.Run("traversal deployment names are rejected at the boundary", func(t *testing.T) {
		for _, path := range []string{
			"/v1/deployments/%2E%2E/backups", // name = ".."
			"/v1/deployments/..%2Fadmin/backups",
			"/v1/deployments/..%2F..%2Fetc/restores",
			"/v1/deployments/db1%2F..%2Fx/backups",
		} {
			// Never 2xx: the name either reaches the handler as a
			// hostile component (400 from the boundary check) or the
			// stdlib normalizes the ".." request-path segment first
			// (404). Either way no storage walk under a hostile key.
			if got := get(path); got == http.StatusOK || got == http.StatusAccepted {
				t.Errorf("GET %s: got %d, want 4xx (traversal must not be accepted)", path, got)
			}
		}
		// %2E%2E is the only form that reaches the handler as the
		// literal name ".." — it must be a clean 400, not a silent
		// 200 + empty list.
		if got := get("/v1/deployments/%2E%2E/backups"); got != http.StatusBadRequest {
			t.Errorf("GET /v1/deployments/%%2E%%2E/backups: got %d, want 400", got)
		}
	})
	t.Run("traversal backup IDs are rejected at enqueue", func(t *testing.T) {
		cases := []struct {
			path, body string
		}{
			{"/v1/deployments/db1/verifies", `{"backup_id":".."}`},
			{"/v1/deployments/db1/verifies", `{"backup_id":"a/b"}`},
			{"/v1/deployments/db1/restores", `{"backup_id":"..","target_dir":"/tmp/restore-f0004"}`},
			{"/v1/deployments/db1/restores", `{"backup_id":"x/y","target_dir":"/tmp/restore-f0004"}`},
		}
		for _, c := range cases {
			if got := post(c.path, c.body); got != http.StatusBadRequest {
				t.Errorf("POST %s %s: got %d, want 400", c.path, c.body, got)
			}
		}
	})

	t.Run("legitimate input keeps working", func(t *testing.T) {
		if got := get("/v1/deployments/db1/backups"); got != http.StatusOK {
			t.Errorf("GET /v1/deployments/db1/backups: got %d, want 200", got)
		}
		// "latest" is a documented sentinel the agent resolves — never a
		// backup ID, so it must not hit the ID validator.
		if got := post("/v1/deployments/db1/verifies", `{"backup_id":"latest"}`); got != http.StatusAccepted {
			t.Errorf("POST verifies latest: got %d, want 202", got)
		}
		if got := post("/v1/deployments/db1/restores",
			`{"backup_id":"db1.full.20260820T120000Z.abcdef01","target_dir":"/tmp/restore-f0004"}`); got != http.StatusAccepted {
			t.Errorf("POST restores valid id: got %d, want 202", got)
		}
	})
}
