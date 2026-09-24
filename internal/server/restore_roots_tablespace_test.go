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

// postRestore posts body to /v1/deployments/db1/restores on a server
// configured with roots and returns status + decoded envelope bytes.
func postRestore(t *testing.T, roots []string, body string) (int, []byte) {
	t.Helper()
	s, err := server.New(server.Config{Listen: "127.0.0.1:0", RestoreRoots: roots})
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(s.Handler())
	defer hs.Close()
	resp, err := http.Post(hs.URL+"/v1/deployments/db1/restores", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// TestEnqueueRestore_TablespaceMappingOutsideRoots pins that
// restore_roots gates EVERY directory a restore writes, not only
// target_dir: a tablespace_mapping destination is a second write root,
// and leaving it unchecked let an API client with a compliant
// target_dir materialise a tablespace into /etc.
func TestEnqueueRestore_TablespaceMappingOutsideRoots(t *testing.T) {
	roots := []string{"/srv/restores"}
	for _, tc := range []struct {
		name    string
		mapping string
	}{
		{"outside", `["/old/ts=/etc/cron.d"]`},
		{"dotdot", `["/old/ts=/srv/restores/../../etc"]`},
		{"relative", `["/old/ts=etc"]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, raw := postRestore(t, roots, `{
				"backup_id": "latest",
				"target_dir": "/srv/restores/db1",
				"tablespace_mapping": `+tc.mapping+`
			}`)
			if status != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s; want 400", status, raw)
			}
			if !strings.Contains(string(raw), "usage.bad_tablespace_mapping") {
				t.Errorf("want usage.bad_tablespace_mapping; body=%s", raw)
			}
		})
	}
}

// TestEnqueueRestore_TablespaceMappingInsideRoots: a destination under
// a root is accepted, and the server stamps its configured roots into
// the job so the agent can re-check them (a client cannot supply or
// widen them).
func TestEnqueueRestore_TablespaceMappingInsideRoots(t *testing.T) {
	status, raw := postRestore(t, []string{"/srv/restores"}, `{
		"backup_id": "latest",
		"target_dir": "/srv/restores/db1",
		"tablespace_mapping": ["/old/ts=/srv/restores/db1_ts"],
		"restore_roots": ["/"]
	}`)
	if status != http.StatusAccepted {
		t.Fatalf("status=%d body=%s; want 202", status, raw)
	}
	var env struct {
		Result *server.Job `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(env.Result.Args["restore_roots"])
	if string(got) != `["/srv/restores"]` {
		t.Errorf("Args.restore_roots = %s; want the server's configured roots", got)
	}
}

// TestEnqueueRestore_TablespaceMappingMalformed: shape errors are
// refused at the boundary rather than failing on the agent.
func TestEnqueueRestore_TablespaceMappingMalformed(t *testing.T) {
	status, raw := postRestore(t, nil, `{
		"backup_id": "latest",
		"target_dir": "/srv/restores/db1",
		"tablespace_mapping": "not-an-array"
	}`)
	if status != http.StatusBadRequest || !strings.Contains(string(raw), "usage.bad_tablespace_mapping") {
		t.Fatalf("status=%d body=%s; want 400 usage.bad_tablespace_mapping", status, raw)
	}
}
