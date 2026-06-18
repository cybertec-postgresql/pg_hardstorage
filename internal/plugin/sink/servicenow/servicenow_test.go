package servicenow_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/sink/servicenow"
)

// snowStub mimics enough of the ServiceNow Now Platform Table API
// for the tests: GET /api/now/table/incident returns either zero or
// one incident; POST creates; PATCH appends work_notes.
type snowStub struct {
	t           *testing.T
	searchHits  int
	searchSysID string
	searchQuery atomic.Pointer[string] // last sysparm_query

	createCalls   atomic.Int32
	createPayload atomic.Pointer[map[string]any]

	patchCalls   atomic.Int32
	patchSysID   atomic.Pointer[string]
	patchPayload atomic.Pointer[map[string]any]
}

func (s *snowStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/now/table/incident"):
		q := r.URL.Query().Get("sysparm_query")
		s.searchQuery.Store(&q)
		out := map[string]any{"result": []any{}}
		if s.searchHits > 0 {
			out = map[string]any{"result": []any{
				map[string]any{"sys_id": s.searchSysID, "number": "INC0010001"},
			}}
		}
		body, _ := json.Marshal(out)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)

	case r.Method == http.MethodPost && r.URL.Path == "/api/now/table/incident":
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		s.createCalls.Add(1)
		s.createPayload.Store(&payload)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"result":{"sys_id":"abc","number":"INC0010002"}}`))

	case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/api/now/table/incident/"):
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		s.patchCalls.Add(1)
		s.patchPayload.Store(&payload)
		// /api/now/table/incident/<sys_id>
		parts := strings.Split(r.URL.Path, "/")
		if len(parts) >= 6 {
			id := parts[5]
			s.patchSysID.Store(&id)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"result":{}}`))

	default:
		body, _ := io.ReadAll(r.Body)
		s.t.Logf("unexpected ServiceNow call: %s %s\nbody: %s", r.Method, r.URL.Path, body)
		http.Error(w, "not implemented", http.StatusNotImplemented)
	}
}

func mustBuild(t *testing.T, baseURL string, extra map[string]any) output.Sink {
	t.Helper()
	cfg := map[string]any{
		"instance_url": baseURL,
		"username":     "ops",
		"password":     "secret",
	}
	for k, v := range extra {
		cfg[k] = v
	}
	s, err := servicenow.NewFromSpec(output.SinkSpec{
		Name: "ops-snow", Plugin: "servicenow", Config: cfg,
	})
	if err != nil {
		t.Fatalf("NewFromSpec: %v", err)
	}
	return s
}

// ----- build / config -----

func TestServiceNow_Build_RequiresInstanceURL(t *testing.T) {
	_, err := servicenow.NewFromSpec(output.SinkSpec{
		Name: "s", Plugin: "servicenow",
		Config: map[string]any{"username": "u", "password": "p"},
	})
	if err == nil || !strings.Contains(err.Error(), "instance_url") {
		t.Errorf("expected instance_url error; got %v", err)
	}
}

func TestServiceNow_Auth_RequiresOneShape(t *testing.T) {
	cases := []struct {
		name string
		cfg  map[string]any
		ok   bool
	}{
		{"basic ok", map[string]any{
			"instance_url": "https://x", "username": "a", "password": "b",
		}, true},
		{"bearer ok", map[string]any{
			"instance_url": "https://x", "bearer_token": "tok",
		}, true},
		{"both rejected", map[string]any{
			"instance_url": "https://x", "username": "a", "password": "b",
			"bearer_token": "tok",
		}, false},
		{"neither rejected", map[string]any{
			"instance_url": "https://x",
		}, false},
		{"basic incomplete", map[string]any{
			"instance_url": "https://x", "username": "a",
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := servicenow.NewFromSpec(output.SinkSpec{
				Name: "s", Plugin: "servicenow", Config: c.cfg,
			})
			if c.ok && err != nil {
				t.Errorf("expected ok, got err: %v", err)
			}
			if !c.ok && err == nil {
				t.Errorf("expected error, got nil")
			}
		})
	}
}

func TestServiceNow_Build_RejectsBadStrategy(t *testing.T) {
	_, err := servicenow.NewFromSpec(output.SinkSpec{
		Name: "s", Plugin: "servicenow",
		Config: map[string]any{
			"instance_url": "https://x",
			"username":     "u", "password": "p",
			"ticket_strategy": "rugby",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "rugby") {
		t.Errorf("expected unknown-strategy error; got %v", err)
	}
}

func TestServiceNow_Build_ParsesActiveStates(t *testing.T) {
	for _, raw := range []any{
		[]any{1, 2}, []int{1, 2}, []any{float64(1), float64(2)},
		[]any{"1", "2"},
	} {
		_, err := servicenow.NewFromSpec(output.SinkSpec{
			Name: "s", Plugin: "servicenow",
			Config: map[string]any{
				"instance_url": "https://x",
				"username":     "u", "password": "p",
				"active_states": raw,
			},
		})
		if err != nil {
			t.Errorf("active_states %T(%v) rejected: %v", raw, raw, err)
		}
	}
}

func TestServiceNow_Build_RejectsEmptyActiveStates(t *testing.T) {
	_, err := servicenow.NewFromSpec(output.SinkSpec{
		Name: "s", Plugin: "servicenow",
		Config: map[string]any{
			"instance_url": "https://x",
			"username":     "u", "password": "p",
			"active_states": []any{},
		},
	})
	if err == nil {
		t.Errorf("expected error for empty active_states")
	}
}

// ----- emission -----

func TestServiceNow_Dedupe_AppendsWorkNotesOnExisting(t *testing.T) {
	stub := &snowStub{t: t, searchHits: 1, searchSysID: "sys-42"}
	srv := httptest.NewServer(stub)
	defer srv.Close()

	s := mustBuild(t, srv.URL, nil)
	defer s.Close()

	ev := output.NewEvent(output.SeverityError, "backup", "manifest.replica_failed").
		WithSubject(output.Subject{Deployment: "db1"})
	if err := s.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if stub.createCalls.Load() != 0 {
		t.Errorf("dedupe matched but create was called %d times", stub.createCalls.Load())
	}
	if stub.patchCalls.Load() != 1 {
		t.Errorf("expected 1 PATCH (work_notes); got %d", stub.patchCalls.Load())
	}
	if id := stub.patchSysID.Load(); id == nil || *id != "sys-42" {
		t.Errorf("PATCH sys_id = %v, want sys-42", id)
	}
	// work_notes payload should carry our prefix.
	payload := stub.patchPayload.Load()
	if payload == nil {
		t.Fatal("PATCH payload not captured")
	}
	wn, _ := (*payload)["work_notes"].(string)
	if !strings.Contains(wn, "[pg_hardstorage]") {
		t.Errorf("work_notes missing prefix: %q", wn)
	}
}

func TestServiceNow_Dedupe_CreatesWhenNoMatch(t *testing.T) {
	stub := &snowStub{t: t, searchHits: 0}
	srv := httptest.NewServer(stub)
	defer srv.Close()

	s := mustBuild(t, srv.URL, nil)
	defer s.Close()

	ev := output.NewEvent(output.SeverityCritical, "kms", "key_destroyed")
	if err := s.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if stub.createCalls.Load() != 1 {
		t.Errorf("expected 1 create; got %d", stub.createCalls.Load())
	}
	// urgency/impact for SeverityCritical should be (1,1).
	payload := stub.createPayload.Load()
	if payload == nil {
		t.Fatal("create payload not captured")
	}
	if u := numField(*payload, "urgency"); u != 1 {
		t.Errorf("urgency = %v, want 1 (critical)", u)
	}
	if i := numField(*payload, "impact"); i != 1 {
		t.Errorf("impact = %v, want 1 (critical)", i)
	}
}

func TestServiceNow_AlwaysNew_AlwaysCreates(t *testing.T) {
	stub := &snowStub{t: t, searchHits: 1, searchSysID: "sys-42"}
	srv := httptest.NewServer(stub)
	defer srv.Close()

	s := mustBuild(t, srv.URL, map[string]any{"ticket_strategy": "always_new"})
	defer s.Close()

	for i := 0; i < 3; i++ {
		_ = s.Emit(context.Background(),
			output.NewEvent(output.SeverityError, "x", "y"))
	}
	if stub.createCalls.Load() != 3 {
		t.Errorf("always_new should create on every emit; got %d", stub.createCalls.Load())
	}
	if stub.patchCalls.Load() != 0 {
		t.Errorf("always_new should never PATCH; got %d", stub.patchCalls.Load())
	}
}

func TestServiceNow_FiltersBelowMinSeverity(t *testing.T) {
	stub := &snowStub{t: t}
	srv := httptest.NewServer(stub)
	defer srv.Close()

	s := mustBuild(t, srv.URL, map[string]any{"min_severity": "critical"})
	defer s.Close()

	if err := s.Emit(context.Background(),
		output.NewEvent(output.SeverityError, "x", "y")); err != nil {
		t.Fatal(err)
	}
	if stub.createCalls.Load() != 0 || stub.patchCalls.Load() != 0 {
		t.Errorf("error event below 'critical' threshold should be dropped")
	}
}

// TestServiceNow_SeverityToUrgencyImpact_Mapping spot-checks the
// non-critical mappings — the create-payload assertions in
// TestServiceNow_Dedupe_CreatesWhenNoMatch cover the critical path.
func TestServiceNow_SeverityToUrgencyImpact_Mapping(t *testing.T) {
	cases := []struct {
		sev      output.Severity
		urgency  int
		impact   int
		creating bool
	}{
		{output.SeverityCritical, 1, 1, true},
		{output.SeverityError, 2, 2, true},
		{output.SeverityWarning, 3, 2, true},
	}
	for _, c := range cases {
		stub := &snowStub{t: t, searchHits: 0}
		srv := httptest.NewServer(stub)
		s := mustBuild(t, srv.URL, map[string]any{"min_severity": "warning"})

		if err := s.Emit(context.Background(),
			output.NewEvent(c.sev, "x", "y")); err != nil {
			t.Errorf("severity %v Emit: %v", c.sev, err)
		}
		if !c.creating {
			s.Close()
			srv.Close()
			continue
		}
		payload := stub.createPayload.Load()
		if payload == nil {
			t.Errorf("severity %v: payload not captured", c.sev)
			s.Close()
			srv.Close()
			continue
		}
		if u := numField(*payload, "urgency"); u != c.urgency {
			t.Errorf("severity %v urgency = %v, want %d", c.sev, u, c.urgency)
		}
		if i := numField(*payload, "impact"); i != c.impact {
			t.Errorf("severity %v impact = %v, want %d", c.sev, i, c.impact)
		}
		s.Close()
		srv.Close()
	}
}

// TestServiceNow_Search_EscapesQuerySpecials defends against query
// injection: a crafted Op containing `^` (the AND separator) or `=`
// (the equality operator) must be stripped from the sysparm_query
// — otherwise an attacker's payload can fold into extra clauses
// and broaden the lookup.
func TestServiceNow_Search_EscapesQuerySpecials(t *testing.T) {
	stub := &snowStub{t: t, searchHits: 0}
	srv := httptest.NewServer(stub)
	defer srv.Close()

	s := mustBuild(t, srv.URL, nil)
	defer s.Close()

	// Op carries the classic injection vector for ServiceNow's query
	// language (^ to add a clause, = to start a new comparison).
	ev := output.NewEvent(output.SeverityError, "backup", `evil^state=999`).
		WithSubject(output.Subject{Deployment: "db1"})
	if err := s.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	q := stub.searchQuery.Load()
	if q == nil {
		t.Fatal("search wasn't called; query not captured")
	}
	// The crafted clause is `state=999` after a `^`; sanitisation
	// should have stripped the metacharacters.  Specifically: there
	// must be exactly one `^` (between the short_description clause
	// and the state-IN clause), and the attacker's `=999` substring
	// must not survive verbatim.
	caretCount := strings.Count(*q, "^")
	if caretCount != 1 {
		t.Errorf("expected exactly one ^ in query (between clauses); got %d in %q", caretCount, *q)
	}
	if strings.Contains(*q, "state=999") {
		t.Errorf("attacker payload survived sanitisation: %q", *q)
	}
}

func TestServiceNow_RegistersWithDefaultRegistry(t *testing.T) {
	found := false
	for _, p := range output.DefaultSinkRegistry.Plugins() {
		if p == "servicenow" {
			found = true
		}
	}
	if !found {
		t.Errorf("servicenow should self-register")
	}
}

// numField extracts a numeric field from the payload, accepting
// both float64 (JSON's default decoded numeric) and int.
func numField(payload map[string]any, key string) int {
	v, ok := payload[key]
	if !ok {
		return -1
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return -1
}
