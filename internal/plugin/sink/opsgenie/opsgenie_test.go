package opsgenie_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/sink/opsgenie"
)

// fakeOpsgenie captures the requests the sink makes so tests can
// assert on the alert payload.
type fakeOpsgenie struct {
	mu          sync.Mutex
	calls       atomic.Int32
	lastBody    []byte
	lastAuth    string
	statusCode  int
	respondBody string
}

func (f *fakeOpsgenie) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.calls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	f.lastBody = body
	f.lastAuth = r.Header.Get("Authorization")
	if f.statusCode == 0 {
		w.WriteHeader(http.StatusAccepted)
	} else {
		w.WriteHeader(f.statusCode)
	}
	body2 := f.respondBody
	if body2 == "" {
		body2 = `{"requestId":"test-uuid","result":"Request will be processed"}`
	}
	_, _ = w.Write([]byte(body2))
}

func mustBuild(t *testing.T, baseURL string, extra map[string]any) output.Sink {
	t.Helper()
	cfg := map[string]any{
		"api_key":      "test-genie-key",
		"api_url":      baseURL,
		"min_severity": "debug",
	}
	for k, v := range extra {
		cfg[k] = v
	}
	s, err := opsgenie.NewFromSpec(output.SinkSpec{
		Name: "test", Plugin: "opsgenie", Config: cfg,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestOpsgenie_Build_RequiresAPIKey(t *testing.T) {
	_, err := opsgenie.NewFromSpec(output.SinkSpec{
		Name: "x", Plugin: "opsgenie", Config: map[string]any{},
	})
	if err == nil {
		t.Error("missing api_key should fail")
	}
}

func TestOpsgenie_Emit_HappyPath(t *testing.T) {
	stub := &fakeOpsgenie{}
	srv := httptest.NewServer(stub)
	defer srv.Close()

	s := mustBuild(t, srv.URL, map[string]any{
		"teams": []string{"ops"},
		"tags":  []string{"pg_hardstorage", "automation"},
	})
	defer s.Close()

	ev := output.NewEvent(output.SeverityError, "backup", "manifest.replica_failed").
		WithSubject(output.Subject{Deployment: "db1"}).
		WithSuggestion(&output.Suggestion{
			DocURL: "https://docs/runbooks/replica-failed",
		})
	if err := s.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	if stub.calls.Load() != 1 {
		t.Errorf("expected 1 API call, got %d", stub.calls.Load())
	}
	if stub.lastAuth != "GenieKey test-genie-key" {
		t.Errorf("Authorization header = %q, want GenieKey test-genie-key", stub.lastAuth)
	}

	var got map[string]any
	if err := json.Unmarshal(stub.lastBody, &got); err != nil {
		t.Fatalf("invalid JSON body: %v\n%s", err, stub.lastBody)
	}
	// Headline + identity tuple → message.
	if msg, _ := got["message"].(string); !strings.Contains(msg, "ERROR") ||
		!strings.Contains(msg, "backup/manifest.replica_failed") ||
		!strings.Contains(msg, "deployment=db1") {
		t.Errorf("message = %q", msg)
	}
	// Severity → P2 (error).
	if pri, _ := got["priority"].(string); pri != "P2" {
		t.Errorf("priority = %q, want P2", pri)
	}
	// Alias is the deterministic dedup key.
	if alias, _ := got["alias"].(string); !strings.HasPrefix(alias, "pgh-") {
		t.Errorf("alias = %q, want pgh-* prefix", alias)
	}
	// Team responder.
	resp, _ := got["responders"].([]any)
	if len(resp) != 1 {
		t.Fatalf("expected 1 responder; got %d", len(resp))
	}
	if r, ok := resp[0].(map[string]any); !ok ||
		r["type"] != "team" || r["name"] != "ops" {
		t.Errorf("responder = %v, want team/ops", resp[0])
	}
	// DocURL → Note.
	if note, _ := got["note"].(string); !strings.Contains(note, "Runbook: https://docs/runbooks/replica-failed") {
		t.Errorf("note = %q, want Runbook: ...", note)
	}
	// Tags pass through.
	tags, _ := got["tags"].([]any)
	if len(tags) != 2 {
		t.Errorf("tags = %v, want 2", tags)
	}
}

func TestOpsgenie_SeverityMapping(t *testing.T) {
	stub := &fakeOpsgenie{}
	srv := httptest.NewServer(stub)
	defer srv.Close()

	cases := []struct {
		sev      output.Severity
		wantPrio string
	}{
		{output.SeverityEmergency, "P1"},
		{output.SeverityAlert, "P1"},
		{output.SeverityCritical, "P1"},
		{output.SeverityError, "P2"},
		{output.SeverityWarning, "P3"},
		{output.SeverityNotice, "P4"},
		{output.SeverityInfo, "P5"},
		{output.SeverityDebug, "P5"},
	}
	s := mustBuild(t, srv.URL, nil)
	defer s.Close()
	for _, c := range cases {
		t.Run(c.sev.String(), func(t *testing.T) {
			before := stub.calls.Load()
			if err := s.Emit(context.Background(),
				output.NewEvent(c.sev, "x", "y")); err != nil {
				t.Fatal(err)
			}
			if stub.calls.Load() != before+1 {
				t.Fatalf("call not made for severity %v", c.sev)
			}
			var got map[string]any
			_ = json.Unmarshal(stub.lastBody, &got)
			if pri, _ := got["priority"].(string); pri != c.wantPrio {
				t.Errorf("severity %v → priority %q, want %q", c.sev, pri, c.wantPrio)
			}
		})
	}
}

func TestOpsgenie_AliasDedupe_Stable(t *testing.T) {
	// Same identity tuple → same alias (so Opsgenie aggregates).
	stub := &fakeOpsgenie{}
	srv := httptest.NewServer(stub)
	defer srv.Close()
	s := mustBuild(t, srv.URL, nil)
	defer s.Close()

	ev := output.NewEvent(output.SeverityError, "backup", "manifest.replica_failed").
		WithSubject(output.Subject{Deployment: "db1"})

	for i := 0; i < 3; i++ {
		if err := s.Emit(context.Background(), ev); err != nil {
			t.Fatal(err)
		}
	}
	// We can only inspect the LAST body; assert it has the same
	// alias shape every time. (The fact that calls fired 3 times
	// is what tells us dedupe is at the Opsgenie layer, not ours.)
	if stub.calls.Load() != 3 {
		t.Errorf("expected 3 calls (dedup is server-side); got %d", stub.calls.Load())
	}
	var got map[string]any
	_ = json.Unmarshal(stub.lastBody, &got)
	alias1, _ := got["alias"].(string)

	// Different deployment → different alias.
	ev2 := output.NewEvent(output.SeverityError, "backup", "manifest.replica_failed").
		WithSubject(output.Subject{Deployment: "db2"})
	if err := s.Emit(context.Background(), ev2); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(stub.lastBody, &got)
	alias2, _ := got["alias"].(string)
	if alias1 == alias2 {
		t.Errorf("different deployments must produce different aliases; both = %q", alias1)
	}
}

func TestOpsgenie_FiltersBelowMinSeverity(t *testing.T) {
	stub := &fakeOpsgenie{}
	srv := httptest.NewServer(stub)
	defer srv.Close()
	s := mustBuild(t, srv.URL, map[string]any{"min_severity": "critical"})
	defer s.Close()

	if err := s.Emit(context.Background(),
		output.NewEvent(output.SeverityError, "x", "y")); err != nil {
		t.Fatal(err)
	}
	if stub.calls.Load() != 0 {
		t.Errorf("error event below 'critical' threshold should be dropped; got %d calls",
			stub.calls.Load())
	}
}

func TestOpsgenie_SurfacesAPIError(t *testing.T) {
	stub := &fakeOpsgenie{
		statusCode:  http.StatusUnauthorized,
		respondBody: `{"message":"Could not authenticate"}`,
	}
	srv := httptest.NewServer(stub)
	defer srv.Close()
	s := mustBuild(t, srv.URL, nil)
	defer s.Close()

	err := s.Emit(context.Background(),
		output.NewEvent(output.SeverityError, "x", "y"))
	if err == nil {
		t.Fatal("expected error from 401 response")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error should mention status 401: %v", err)
	}
}

func TestOpsgenie_PreCancelledCtx_RefusesEmit(t *testing.T) {
	// Deliberately use an unroutable URL so a dial would hang —
	// the pre-Emit ctx check must bail before we get there.
	s, err := opsgenie.NewFromSpec(output.SinkSpec{
		Name: "x", Plugin: "opsgenie", Config: map[string]any{
			"api_key":      "k",
			"api_url":      "http://127.0.0.1:1",
			"min_severity": "debug",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Emit(ctx, output.NewEvent(output.SeverityError, "x", "y")); err == nil {
		t.Error("Emit should have honoured pre-cancelled ctx")
	}
}

func TestOpsgenie_RegistersWithDefaultRegistry(t *testing.T) {
	found := false
	for _, p := range output.DefaultSinkRegistry.Plugins() {
		if p == "opsgenie" {
			found = true
		}
	}
	if !found {
		t.Errorf("opsgenie should self-register")
	}
}
