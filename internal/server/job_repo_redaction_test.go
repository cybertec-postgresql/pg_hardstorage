package server_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/server"
)

// TestJobAPI_RedactsRepoCredentials pins M44: job responses echoed the
// repo URL verbatim -- sftp passwords, azblob ?sig= SAS tokens -- to any
// API client, in the enqueue response and in GET /v1/jobs[/<id>]. The
// operator-facing views now carry the redacted URL; only the agent's
// claim still receives the real one, because the agent opens the repo
// with it.
func TestJobAPI_RedactsRepoCredentials(t *testing.T) {
	const secretRepo = "sftp://backup:hunter2@10.0.0.9/srv/repo?sig=SASSECRET"
	s, err := server.New(server.Config{Listen: "127.0.0.1:0", Repos: []string{secretRepo}})
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(s.Handler())
	defer hs.Close()

	check := func(what, body string) {
		t.Helper()
		for _, secret := range []string{"hunter2", "SASSECRET"} {
			if strings.Contains(body, secret) {
				t.Errorf("%s leaks %q:\n%s", what, secret, body)
			}
		}
	}
	post := func(path, body string) string {
		resp, err := http.Post(hs.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return string(raw)
	}
	check("backup enqueue", post("/v1/deployments/db1/backups", `{"repo": "`+secretRepo+`"}`))
	check("restore enqueue", post("/v1/deployments/db1/restores", `{"backup_id":"latest","target_dir":"/srv/r"}`))

	jobs, err := s.Jobs().List(server.ListOptions{})
	if err != nil || len(jobs) != 2 {
		t.Fatalf("jobs = %d, %v", len(jobs), err)
	}
	_, list := get(t, hs.URL, "/v1/jobs")
	check("GET /v1/jobs", list)
	_, one := get(t, hs.URL, "/v1/jobs/"+jobs[0].ID)
	check("GET /v1/jobs/<id>", one)

	// The claim is the agent's channel and must keep the real URL.
	resp, err := http.Post(hs.URL+"/v1/jobs/claim", "application/json",
		strings.NewReader(`{"agent_id":"a1","deployments":["db1"]}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(raw), "hunter2") {
		t.Errorf("claim must carry the real repo URL for the agent to open it:\n%s", raw)
	}
	check("cancel response", post("/v1/jobs/"+jobs[0].ID+"/cancel", `{}`))
}
