package server_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/server"
)

// TestReadyz_RedactsRepoCredentials pins that the UNAUTHENTICATED readiness
// probe never echoes repo-URL credentials — neither the userinfo password
// nor a query-string signature — in the repo URL it names OR in the
// backend error it surfaces. An unknown scheme fails fast (no network) and
// repo.Open wraps the raw URL into its error, exercising both redaction
// paths.
func TestReadyz_RedactsRepoCredentials(t *testing.T) {
	const password = "secretpassword"
	const sig = "TOPSECRETSIGNATURE"
	repoURL := "bogus://admin:" + password + "@db.internal:5432/repo?sig=" + sig

	s, err := server.New(server.Config{Listen: "127.0.0.1:0", Repos: []string{repoURL}})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/readyz", nil) // no Authorization header
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	body := w.Body.String()
	if strings.Contains(body, password) {
		t.Errorf("readyz leaked the repo password to an unauthenticated caller; body=%s", body)
	}
	if strings.Contains(body, sig) {
		t.Errorf("readyz leaked the query-string signature to an unauthenticated caller; body=%s", body)
	}
	// The redacted URL should still name the host so operators can identify
	// which repo is degraded, and show the password was masked.
	if !strings.Contains(body, "db.internal") {
		t.Errorf("readyz should still surface the (redacted) repo host; body=%s", body)
	}
	if !strings.Contains(body, "xxxxx") {
		t.Errorf("readyz should show the password masked as xxxxx; body=%s", body)
	}
	// The bogus repo can't open, so the probe is degraded.
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}
