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

// TestEnqueueVerify_HappyPath asserts POST /v1/deployments/<n>/verifies
// enqueues a JobVerify with body fields captured into Args.
func TestEnqueueVerify_HappyPath(t *testing.T) {
	s, err := server.New(server.Config{Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(s.Handler())
	defer hs.Close()

	body := `{"backup_id": "db1.full.20260427T0900Z", "pg_major": "17"}`
	resp, err := http.Post(hs.URL+"/v1/deployments/db1/verifies",
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
	if env.Result.Kind != server.JobVerify {
		t.Errorf("Kind = %q, want verify", env.Result.Kind)
	}
	if env.Result.Args["backup_id"] != "db1.full.20260427T0900Z" {
		t.Errorf("Args.backup_id = %v", env.Result.Args["backup_id"])
	}
	if env.Result.Args["pg_major"] != "17" {
		t.Errorf("Args.pg_major = %v", env.Result.Args["pg_major"])
	}
}

// TestEnqueueVerify_RequiresBackupID — symmetric to the restore route.
func TestEnqueueVerify_RequiresBackupID(t *testing.T) {
	s, err := server.New(server.Config{Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(s.Handler())
	defer hs.Close()

	resp, err := http.Post(hs.URL+"/v1/deployments/db1/verifies",
		"application/json", strings.NewReader(`{}`))
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
		t.Errorf("error should name field; got %s", raw)
	}
}

// TestEnqueueVerify_RequiresBody asserts the empty-body refusal.
func TestEnqueueVerify_RequiresBody(t *testing.T) {
	s, err := server.New(server.Config{Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(s.Handler())
	defer hs.Close()

	resp, err := http.Post(hs.URL+"/v1/deployments/db1/verifies",
		"application/json", strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

// TestEnqueueVerify_OnlyPOST refuses other methods.
func TestEnqueueVerify_OnlyPOST(t *testing.T) {
	s, err := server.New(server.Config{Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(s.Handler())
	defer hs.Close()

	resp, err := http.Get(hs.URL + "/v1/deployments/db1/verifies")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d, want 405", resp.StatusCode)
	}
}

// TestEnqueueVerify_DispatchableViaClaim asserts an agent claiming
// with Kinds=[verify] gets the queued verify job.
func TestEnqueueVerify_DispatchableViaClaim(t *testing.T) {
	s, err := server.New(server.Config{Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(s.Handler())
	defer hs.Close()

	body := `{"backup_id": "x"}`
	if r, err := http.Post(hs.URL+"/v1/deployments/db1/verifies",
		"application/json", strings.NewReader(body)); err != nil {
		t.Fatal(err)
	} else {
		r.Body.Close()
	}

	claim := `{"agent_id":"agent-1","deployments":["db1"],"kinds":["verify"]}`
	resp, err := http.Post(hs.URL+"/v1/jobs/claim", "application/json", strings.NewReader(claim))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("claim: status=%d body=%s", resp.StatusCode, raw)
	}
}
