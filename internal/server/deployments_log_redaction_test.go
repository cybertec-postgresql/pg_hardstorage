package server_test

// SEC-3: repo URLs configured for the control plane routinely embed
// userinfo (sftp://user:pass@host) or query-string credentials
// (azure ?sig=). The unauthenticated readyz path scrubs them
// (redactRepoURL/redactRepoErr); the deployments handler logged the
// RAW url + error on every transient repo-open failure, landing the
// plaintext password in whatever aggregates the control plane's
// stderr (journald, SIEM, log shipper).

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/server"
)

type captureLogger struct {
	mu    sync.Mutex
	lines []string
}

func (c *captureLogger) record(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, fmt.Sprintf(format, args...))
}

func (c *captureLogger) Infof(format string, args ...any)  { c.record(format, args...) }
func (c *captureLogger) Errorf(format string, args ...any) { c.record(format, args...) }

func (c *captureLogger) joined() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.lines, "\n")
}

func TestDeployments_LogRedactsRepoCredentials(t *testing.T) {
	log := &captureLogger{}
	s, err := server.New(server.Config{
		// .invalid is a reserved TLD: guaranteed unresolvable, so
		// repo.Open fails deterministically, and the sftp open-error
		// path carries the raw URL.
		Repos: []string{"sftp://backup-user:secret-kek-9@nas.invalid/backups"},
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	s = s.WithLogger(log)

	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/deployments")
	if err != nil {
		t.Fatalf("GET /v1/deployments: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/deployments: status %d, want 200 (repo errors are logged, not fatal)", resp.StatusCode)
	}

	captured := log.joined()
	if captured == "" {
		t.Fatal("expected the repo-open failure to be logged; nothing captured")
	}
	if strings.Contains(captured, "secret-kek-9") {
		t.Errorf("control-plane log leaked the repo password:\n%s", captured)
	}
	if !strings.Contains(captured, "nas.invalid") {
		t.Errorf("log should still name the repo host for diagnosis:\n%s", captured)
	}
}
