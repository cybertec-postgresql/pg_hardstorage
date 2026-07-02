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

// newChunkedPOST builds a POST whose body has an unknown length, the
// way a chunked transfer-encoded request arrives on the wire
// (ContentLength == -1). Wrapping the reader in io.NopCloser hides the
// concrete *strings.Reader from net/http so it does NOT auto-populate
// ContentLength, forcing chunked framing.
func newChunkedPOST(t *testing.T, url, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, io.NopCloser(strings.NewReader(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = -1 // unknown length → chunked
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// TestEnqueue_ChunkedBodies is the regression for bug #21:
// handleEnqueueBackup only parsed the body when r.ContentLength>0 (so
// a chunked POST's args were silently dropped), and
// handleEnqueueVerify/Restore rejected ContentLength<=0 outright (so a
// valid chunked body 400'd). All three must handle chunked/
// unknown-length bodies: parse when a body is present, only 400 when
// genuinely absent or invalid.
func TestEnqueue_ChunkedBodies(t *testing.T) {
	s, err := server.New(server.Config{Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(s.Handler())
	defer hs.Close()

	t.Run("backup_chunked_args_preserved", func(t *testing.T) {
		resp := newChunkedPOST(t, hs.URL+"/v1/deployments/db1/backups",
			`{"repo":"file:///srv/repo","tag":"nightly"}`)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			raw, _ := io.ReadAll(resp.Body)
			t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
		}
		var env struct {
			Result *server.Job `json:"result"`
		}
		mustDecode(t, resp.Body, &env)
		if env.Result == nil {
			t.Fatal("no result")
		}
		// The chunked body's args must have reached the job (they were
		// dropped before the fix because ContentLength was -1).
		if env.Result.Args["tag"] != "nightly" {
			t.Errorf("chunked backup args dropped: %+v", env.Result.Args)
		}
	})

	t.Run("verify_chunked_body_accepted", func(t *testing.T) {
		resp := newChunkedPOST(t, hs.URL+"/v1/deployments/db1/verifies",
			`{"backup_id":"db1.full.20260427T0900Z"}`)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			raw, _ := io.ReadAll(resp.Body)
			t.Fatalf("verify chunked body rejected: status=%d body=%s", resp.StatusCode, raw)
		}
	})

	t.Run("restore_chunked_body_accepted", func(t *testing.T) {
		resp := newChunkedPOST(t, hs.URL+"/v1/deployments/db1/restores",
			`{"backup_id":"db1.full.20260427T0900Z","target_dir":"/var/lib/postgresql/restored"}`)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			raw, _ := io.ReadAll(resp.Body)
			t.Fatalf("restore chunked body rejected: status=%d body=%s", resp.StatusCode, raw)
		}
	})

	// A genuinely absent body still 400s for the body-required routes.
	t.Run("verify_empty_body_still_400", func(t *testing.T) {
		resp := newChunkedPOST(t, hs.URL+"/v1/deployments/db1/verifies", "")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("empty verify body: status=%d, want 400", resp.StatusCode)
		}
	})

	t.Run("restore_empty_body_still_400", func(t *testing.T) {
		resp := newChunkedPOST(t, hs.URL+"/v1/deployments/db1/restores", "")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("empty restore body: status=%d, want 400", resp.StatusCode)
		}
	})
}

func mustDecode(t *testing.T, r io.Reader, dst any) {
	t.Helper()
	raw, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		t.Fatalf("decode: %v\n%s", err, raw)
	}
}
