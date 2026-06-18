package server_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/server"
)

// TestEnqueueRestore_HappyPath asserts the new POST
// /v1/deployments/<n>/restores endpoint enqueues a JobRestore with
// the body captured into Args.
func TestEnqueueRestore_HappyPath(t *testing.T) {
	s, err := server.New(server.Config{Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(s.Handler())
	defer hs.Close()

	body := `{
		"backup_id":  "db1.full.20260427T0900Z",
		"target_dir": "/var/lib/postgresql/restored",
		"to":         "5 minutes ago",
		"to_action":  "pause"
	}`
	resp, err := http.Post(hs.URL+"/v1/deployments/db1/restores",
		"application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}

	var env struct {
		Result *server.Job `json:"result"`
	}
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode: %v\n%s", err, raw)
	}
	if env.Result == nil {
		t.Fatalf("no result: %s", raw)
	}
	if env.Result.Kind != server.JobRestore {
		t.Errorf("Kind = %q, want restore", env.Result.Kind)
	}
	if env.Result.Deployment != "db1" {
		t.Errorf("Deployment = %q", env.Result.Deployment)
	}
	if env.Result.State != server.JobQueued {
		t.Errorf("State = %q, want queued", env.Result.State)
	}
	if env.Result.Args["backup_id"] != "db1.full.20260427T0900Z" {
		t.Errorf("Args.backup_id missing: %+v", env.Result.Args)
	}
	if env.Result.Args["target_dir"] != "/var/lib/postgresql/restored" {
		t.Errorf("Args.target_dir missing: %+v", env.Result.Args)
	}
}

// TestEnqueueRestore_RequiresBody asserts the route refuses an empty
// body with a structured error code so the operator's CLI can act on
// it.
func TestEnqueueRestore_RequiresBody(t *testing.T) {
	s, err := server.New(server.Config{Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(s.Handler())
	defer hs.Close()

	resp, err := http.Post(hs.URL+"/v1/deployments/db1/restores",
		"application/json", strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "usage.bad_body") {
		t.Errorf("expected usage.bad_body code; got %s", body)
	}
}

// TestEnqueueRestore_RequiresBackupID asserts the body validation
// surfaces a structured missing-field error.
func TestEnqueueRestore_RequiresBackupID(t *testing.T) {
	s, err := server.New(server.Config{Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(s.Handler())
	defer hs.Close()

	body := `{"target_dir": "/tmp/x"}` // backup_id missing
	resp, err := http.Post(hs.URL+"/v1/deployments/db1/restores",
		"application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "usage.missing_field") {
		t.Errorf("expected usage.missing_field; got %s", raw)
	}
	if !strings.Contains(string(raw), "backup_id") {
		t.Errorf("error message should name the missing field; got %s", raw)
	}
}

// TestEnqueueRestore_RequiresTargetDir asserts the second required field.
func TestEnqueueRestore_RequiresTargetDir(t *testing.T) {
	s, err := server.New(server.Config{Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(s.Handler())
	defer hs.Close()

	body := `{"backup_id": "x"}` // target_dir missing
	resp, err := http.Post(hs.URL+"/v1/deployments/db1/restores",
		"application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "target_dir") {
		t.Errorf("error message should name the missing field; got %s", raw)
	}
}

// TestEnqueueRestore_OnlyPOST refuses GET/PUT/DELETE.
func TestEnqueueRestore_OnlyPOST(t *testing.T) {
	s, err := server.New(server.Config{Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(s.Handler())
	defer hs.Close()

	resp, err := http.Get(hs.URL + "/v1/deployments/db1/restores")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d, want 405", resp.StatusCode)
	}
}

// TestEnqueueRestore_DispatchableViaClaim — end-to-end. After enqueue,
// an agent claiming with Kinds=[restore] should pick it up. This is
// the v0.5 contract: backup + restore both flow through the same
// claim/progress/complete loop.
func TestEnqueueRestore_DispatchableViaClaim(t *testing.T) {
	s, err := server.New(server.Config{Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(s.Handler())
	defer hs.Close()

	body := `{"backup_id": "x", "target_dir": "/tmp/x"}`
	if r, err := http.Post(hs.URL+"/v1/deployments/db1/restores",
		"application/json", strings.NewReader(body)); err != nil {
		t.Fatal(err)
	} else {
		r.Body.Close()
	}

	// Claim with Kinds=[restore].
	claim := `{"agent_id":"agent-1","deployments":["db1"],"kinds":["restore"]}`
	resp, err := http.Post(hs.URL+"/v1/jobs/claim", "application/json", strings.NewReader(claim))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("claim: status=%d body=%s", resp.StatusCode, raw)
	}

	// Now claim with Kinds=[backup] — should get no_jobs because
	// the only queued job is a restore.
	claim2 := `{"agent_id":"agent-2","deployments":["db1"],"kinds":["backup"]}`
	resp2, err := http.Post(hs.URL+"/v1/jobs/claim", "application/json", strings.NewReader(claim2))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		raw, _ := io.ReadAll(resp2.Body)
		t.Errorf("kind-mismatched claim should be 404 no_jobs; got %d body=%s", resp2.StatusCode, raw)
	}
}

// TestEnqueueRestore_PITRTypes covers the defence-in-depth validation
// for the PG-typed PITR fields: to_lsn, to_action, to_timeline. Bad
// values must be rejected at the route boundary so an operator sees a
// precise local error instead of a queued job that fails on the agent.
func TestEnqueueRestore_PITRTypes(t *testing.T) {
	s, err := server.New(server.Config{Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(s.Handler())
	defer hs.Close()

	cases := []struct {
		name     string
		body     string
		wantCode string
	}{
		{
			name:     "to_lsn garbage",
			body:     `{"backup_id":"db1.full.X","target_dir":"/var/lib/postgresql/r","to_lsn":"hm"}`,
			wantCode: "usage.bad_lsn",
		},
		{
			name:     "to_lsn trailing garbage",
			body:     `{"backup_id":"db1.full.X","target_dir":"/var/lib/postgresql/r","to_lsn":"0/3000028x"}`,
			wantCode: "usage.bad_lsn",
		},
		{
			name:     "to_action typo",
			body:     `{"backup_id":"db1.full.X","target_dir":"/var/lib/postgresql/r","to_action":"resume"}`,
			wantCode: "usage.bad_action",
		},
		{
			name:     "to_timeline non-numeric",
			body:     `{"backup_id":"db1.full.X","target_dir":"/var/lib/postgresql/r","to_timeline":"foo"}`,
			wantCode: "usage.bad_timeline",
		},
		{
			name:     "to_timeline zero",
			body:     `{"backup_id":"db1.full.X","target_dir":"/var/lib/postgresql/r","to_timeline":"0"}`,
			wantCode: "usage.bad_timeline",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, err := http.Post(hs.URL+"/v1/deployments/db1/restores",
				"application/json", strings.NewReader(c.body))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			raw, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status=%d want 400; body=%s", resp.StatusCode, raw)
			}
			var env struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(raw, &env); err != nil {
				t.Fatalf("decode: %v\n%s", err, raw)
			}
			if env.Error.Code != c.wantCode {
				t.Errorf("code=%q want %q; body=%s", env.Error.Code, c.wantCode, raw)
			}
		})
	}
}

// TestEnqueueRestore_PITRTypes_HappyPath confirms the new validator
// does not block legitimate values.
func TestEnqueueRestore_PITRTypes_HappyPath(t *testing.T) {
	s, err := server.New(server.Config{Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(s.Handler())
	defer hs.Close()

	body := `{
		"backup_id":  "db1.full.20260427T0900Z",
		"target_dir": "/var/lib/postgresql/restored",
		"to_lsn":     "0/3000028",
		"to_action":  "promote",
		"to_timeline":"7"
	}`
	resp, err := http.Post(hs.URL+"/v1/deployments/db1/restores",
		"application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
}
