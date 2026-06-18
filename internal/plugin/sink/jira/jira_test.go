package jira_test

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
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/sink/jira"
)

// jiraStub mimics enough of the JIRA Cloud REST API for our tests:
// search returns either zero or one issue (configurable), POST
// /issue + POST /issue/{key}/comment record what they got.
type jiraStub struct {
	t              *testing.T
	searchHits     int
	searchHitKey   string
	createCalls    atomic.Int32
	commentCalls   atomic.Int32
	lastCreatePath atomic.Pointer[string]
	lastCommentKey atomic.Pointer[string]
}

func (s *jiraStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/rest/api/3/search"):
		out := map[string]any{"issues": []any{}}
		if s.searchHits > 0 {
			out = map[string]any{"issues": []any{
				map[string]any{"key": s.searchHitKey},
			}}
		}
		body, _ := json.Marshal(out)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)

	case r.Method == http.MethodPost && r.URL.Path == "/rest/api/3/issue":
		s.createCalls.Add(1)
		path := r.URL.Path
		s.lastCreatePath.Store(&path)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"key":"OPS-1"}`))

	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/rest/api/3/issue/") &&
		strings.HasSuffix(r.URL.Path, "/comment"):
		s.commentCalls.Add(1)
		// path is /rest/api/3/issue/<key>/comment — extract key.
		parts := strings.Split(r.URL.Path, "/")
		if len(parts) >= 6 {
			key := parts[5]
			s.lastCommentKey.Store(&key)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))

	default:
		body, _ := io.ReadAll(r.Body)
		s.t.Logf("unexpected JIRA call: %s %s\nbody: %s", r.Method, r.URL.Path, body)
		http.Error(w, "not implemented", http.StatusNotImplemented)
	}
}

func mustBuild(t *testing.T, baseURL string, extra map[string]any) output.Sink {
	t.Helper()
	cfg := map[string]any{
		"base_url":  baseURL,
		"project":   "OPS",
		"email":     "ops@acme.com",
		"api_token": "tok",
	}
	for k, v := range extra {
		cfg[k] = v
	}
	s, err := jira.NewFromSpec(output.SinkSpec{Name: "ops-jira", Plugin: "jira", Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestJira_Build_RequiresBaseURLAndProject(t *testing.T) {
	for _, cfg := range []map[string]any{
		{"project": "OPS"},        // missing base_url
		{"base_url": "https://x"}, // missing project
	} {
		_, err := jira.NewFromSpec(output.SinkSpec{Name: "j", Plugin: "jira", Config: cfg})
		if err == nil {
			t.Errorf("expected error for cfg %v", cfg)
		}
	}
}

func TestJira_Auth_RequiresOneShape(t *testing.T) {
	cases := []struct {
		name string
		cfg  map[string]any
		ok   bool
	}{
		{"basic ok", map[string]any{"base_url": "https://x", "project": "P", "email": "a", "api_token": "b"}, true},
		{"bearer ok", map[string]any{"base_url": "https://x", "project": "P", "bearer_token": "tok"}, true},
		{"both rejected", map[string]any{"base_url": "https://x", "project": "P", "email": "a", "api_token": "b", "bearer_token": "tok"}, false},
		{"neither rejected", map[string]any{"base_url": "https://x", "project": "P"}, false},
		{"basic incomplete", map[string]any{"base_url": "https://x", "project": "P", "email": "a"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := jira.NewFromSpec(output.SinkSpec{Name: "j", Plugin: "jira", Config: c.cfg})
			if c.ok && err != nil {
				t.Errorf("expected ok, got err: %v", err)
			}
			if !c.ok && err == nil {
				t.Errorf("expected error, got nil")
			}
		})
	}
}

func TestJira_Build_RejectsBadStrategy(t *testing.T) {
	_, err := jira.NewFromSpec(output.SinkSpec{
		Name:   "j",
		Plugin: "jira",
		Config: map[string]any{
			"base_url": "https://x", "project": "OPS",
			"email": "a", "api_token": "b",
			"ticket_strategy": "rugby",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "rugby") {
		t.Errorf("expected unknown-strategy error; got %v", err)
	}
}

func TestJira_Dedupe_CommentsOnExisting(t *testing.T) {
	stub := &jiraStub{t: t, searchHits: 1, searchHitKey: "OPS-42"}
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
	if stub.commentCalls.Load() != 1 {
		t.Errorf("expected 1 comment; got %d", stub.commentCalls.Load())
	}
	if k := stub.lastCommentKey.Load(); k == nil || *k != "OPS-42" {
		t.Errorf("comment key = %v, want OPS-42", k)
	}
}

func TestJira_Dedupe_CreatesWhenNoMatch(t *testing.T) {
	stub := &jiraStub{t: t, searchHits: 0}
	srv := httptest.NewServer(stub)
	defer srv.Close()

	s := mustBuild(t, srv.URL, nil)
	defer s.Close()

	ev := output.NewEvent(output.SeverityError, "backup", "manifest.replica_failed").
		WithSubject(output.Subject{Deployment: "db1"})
	if err := s.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if stub.createCalls.Load() != 1 {
		t.Errorf("expected 1 create; got %d", stub.createCalls.Load())
	}
	if stub.commentCalls.Load() != 0 {
		t.Errorf("no match → no comment; got %d", stub.commentCalls.Load())
	}
}

func TestJira_AlwaysNew_AlwaysCreates(t *testing.T) {
	stub := &jiraStub{t: t, searchHits: 1, searchHitKey: "OPS-42"}
	srv := httptest.NewServer(stub)
	defer srv.Close()

	s := mustBuild(t, srv.URL, map[string]any{"ticket_strategy": "always_new"})
	defer s.Close()

	for i := 0; i < 3; i++ {
		_ = s.Emit(context.Background(),
			output.NewEvent(output.SeverityError, "x", "y"))
	}
	if stub.createCalls.Load() != 3 {
		t.Errorf("always_new should create on every emit; got %d creates", stub.createCalls.Load())
	}
	if stub.commentCalls.Load() != 0 {
		t.Errorf("always_new should never comment; got %d", stub.commentCalls.Load())
	}
}

func TestJira_FiltersBelowMinSeverity(t *testing.T) {
	stub := &jiraStub{t: t}
	srv := httptest.NewServer(stub)
	defer srv.Close()

	s := mustBuild(t, srv.URL, map[string]any{"min_severity": "critical"})
	defer s.Close()

	if err := s.Emit(context.Background(),
		output.NewEvent(output.SeverityError, "x", "y")); err != nil {
		t.Fatal(err)
	}
	if stub.createCalls.Load() != 0 || stub.commentCalls.Load() != 0 {
		t.Errorf("error event below 'critical' threshold should be dropped")
	}
}

// A crafted Op that includes a JQL-string-breaking character must
// not produce unescaped quotes in the search URL — that would let
// an attacker control downstream JQL semantics. The fix in
// findExistingIssue routes both project and summary through
// jiraEscape; this end-to-end test asserts the sent URL.
func TestJira_Search_EscapesJQLSpecials(t *testing.T) {
	var lastJQL atomic.Pointer[string]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Capture the jql query parameter and respond with no hits
		// so the sink falls through to create.
		if strings.HasPrefix(r.URL.Path, "/rest/api/3/search") {
			jql := r.URL.Query().Get("jql")
			lastJQL.Store(&jql)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"issues":[]}`))
			return
		}
		// Swallow the create POST so Emit doesn't error after search.
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"key":"OPS-1"}`))
	}))
	defer srv.Close()

	s := mustBuild(t, srv.URL, nil)
	defer s.Close()

	// Op carries the classic injection vector. dedupSummary embeds
	// it into the JQL `summary ~ "<value>"` literal.
	ev := output.NewEvent(output.SeverityError, "backup", `evil" OR project = "OTHER`).
		WithSubject(output.Subject{Deployment: "db1"})
	if err := s.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	jqlPtr := lastJQL.Load()
	if jqlPtr == nil {
		t.Fatal("search wasn't called; JQL not captured")
	}
	jql := *jqlPtr

	// The attacker's payload contained an unescaped " — the JQL
	// must contain the escaped form (\") and nothing else. If a
	// future regression switches the format string to %s without
	// escaping, the JQL would contain the bare " and this test
	// fires.
	if strings.Contains(jql, `evil" OR`) {
		t.Errorf("UNESCAPED \" leaked into JQL:\n  %s", jql)
	}
	if !strings.Contains(jql, `evil\" OR`) {
		t.Errorf("JQL should contain ESCAPED quote (\\\"); got:\n  %s", jql)
	}
	// Defense-in-depth: count parities. Outside any literal, the
	// JQL should contain ONLY the connectives (` AND `, ` ~ `, etc.)
	// and the operator-set project name — not the attacker's payload.
	residual := stripQuotedRegions(jql)
	if strings.Contains(residual, "OR project") {
		t.Errorf("attacker substring leaked into unquoted JQL region:\n  full: %s\n  residual: %s",
			jql, residual)
	}
}

// stripQuotedRegions removes everything between unescaped pairs of
// double-quotes, so what's left is the JQL "outside any literal."
// Used by the injection regression test only.
func stripQuotedRegions(s string) string {
	var out strings.Builder
	inQuote := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		// Count backslashes immediately preceding this char.
		bs := 0
		for j := i - 1; j >= 0 && s[j] == '\\'; j-- {
			bs++
		}
		if c == '"' && bs%2 == 0 {
			inQuote = !inQuote
			continue
		}
		if !inQuote {
			out.WriteByte(c)
		}
	}
	return out.String()
}

func TestJira_RegistersWithDefaultRegistry(t *testing.T) {
	found := false
	for _, p := range output.DefaultSinkRegistry.Plugins() {
		if p == "jira" {
			found = true
		}
	}
	if !found {
		t.Errorf("jira should self-register")
	}
}
