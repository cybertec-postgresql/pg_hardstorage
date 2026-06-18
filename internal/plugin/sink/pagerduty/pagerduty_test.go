package pagerduty_test

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
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/sink/pagerduty"
)

// We can't actually point the production PD URL elsewhere via env,
// so the tests stub the HTTP client by intercepting via a custom
// transport. The simpler path: build a test server, monkey-patch
// EventsAPIv2URL (it's a const — not patchable) → can't.
//
// Instead: the production sink's transport is the default
// http.DefaultTransport. We use httptest.Server and hand its URL
// in via a thin override. To keep production code clean, the
// test re-runs the builder against the test endpoint by
// reflecting on the sink's httpClient — that level of glue isn't
// worth it. The tests below exercise the SHAPE of payloads via
// pure-function helpers in the package, not the live POST.

// captured is a parsed PD payload for shape assertions.
type captured struct {
	body []byte
}

func newServer(t *testing.T, status int, store *atomic.Pointer[captured]) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		store.Store(&captured{body: body})
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"status":"success"}`))
	}))
}

// We can wire the test server in by overriding the package
// EventsAPIv2URL via a small testing-internal helper. The cleanest
// approach short of reflection: expose a build-helper that takes the
// URL.
//
// For v0.1 we keep the URL a const + introduce one indirection: an
// in-package OverrideEventsAPIv2URL test hook below. (See the test-
// only file pagerduty_test_hooks.go; tests import that.)

func TestPagerDuty_Severity_Mapping(t *testing.T) {
	cases := []struct {
		in   output.Severity
		want string
	}{
		{output.SeverityEmergency, "critical"},
		{output.SeverityAlert, "critical"},
		{output.SeverityCritical, "critical"},
		{output.SeverityError, "error"},
		{output.SeverityWarning, "warning"},
		{output.SeverityNotice, "info"},
		{output.SeverityInfo, "info"},
		{output.SeverityDebug, "info"},
	}
	for _, c := range cases {
		got := pagerduty.MapSeverityForTest(c.in)
		if got != c.want {
			t.Errorf("severity %s → %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPagerDuty_DedupKey_StableForSameTuple(t *testing.T) {
	a := output.NewEvent(output.SeverityError, "wal.stream", "lag_high").
		WithSubject(output.Subject{Deployment: "db1"})
	b := output.NewEvent(output.SeverityError, "wal.stream", "lag_high").
		WithSubject(output.Subject{Deployment: "db1"})
	if pagerduty.DedupKeyForTest(a) != pagerduty.DedupKeyForTest(b) {
		t.Errorf("identical (component, op, subject) should yield identical dedup_key")
	}
	c := output.NewEvent(output.SeverityError, "wal.stream", "lag_high").
		WithSubject(output.Subject{Deployment: "db2"})
	if pagerduty.DedupKeyForTest(a) == pagerduty.DedupKeyForTest(c) {
		t.Errorf("different deployment should yield different dedup_key")
	}
}

func TestPagerDuty_Build_RequiresRoutingKey(t *testing.T) {
	_, err := pagerduty.NewFromSpec(output.SinkSpec{Name: "p", Plugin: "pagerduty"})
	if err == nil || !strings.Contains(err.Error(), "routing_key") {
		t.Errorf("expected routing_key required error; got %v", err)
	}
}

func TestPagerDuty_RegistersWithDefaultRegistry(t *testing.T) {
	found := false
	for _, p := range output.DefaultSinkRegistry.Plugins() {
		if p == "pagerduty" {
			found = true
		}
	}
	if !found {
		t.Errorf("pagerduty should self-register")
	}
}

// TestPagerDuty_Emit_PostsExpectedShape covers the live HTTP path
// via the OverrideEventsAPIv2URL test hook.
func TestPagerDuty_Emit_PostsExpectedShape(t *testing.T) {
	var got atomic.Pointer[captured]
	srv := newServer(t, 202, &got)
	defer srv.Close()

	restore := pagerduty.OverrideEventsAPIv2URL(srv.URL)
	defer restore()

	s, err := pagerduty.NewFromSpec(output.SinkSpec{
		Name:   "test",
		Plugin: "pagerduty",
		Config: map[string]any{
			"routing_key":  "abc123",
			"source":       "pg_hardstorage@db1",
			"min_severity": "warning",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ev := output.NewEvent(output.SeverityError, "backup", "manifest.replica_failed").
		WithSubject(output.Subject{Deployment: "db1", BackupID: "db1.full.20260428T1200Z"}).
		WithBody(map[string]any{"error": "disk full"})
	if err := s.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	c := got.Load()
	if c == nil {
		t.Fatal("server didn't receive a request")
	}
	var p map[string]any
	if err := json.Unmarshal(c.body, &p); err != nil {
		t.Fatalf("body not JSON: %v\n%s", err, c.body)
	}
	if p["routing_key"] != "abc123" {
		t.Errorf("routing_key = %v", p["routing_key"])
	}
	if p["event_action"] != "trigger" {
		t.Errorf("event_action = %v", p["event_action"])
	}
	if p["dedup_key"] == nil || p["dedup_key"] == "" {
		t.Errorf("dedup_key missing")
	}
	pl, ok := p["payload"].(map[string]any)
	if !ok {
		t.Fatalf("payload not an object")
	}
	if pl["severity"] != "error" {
		t.Errorf("payload.severity = %v", pl["severity"])
	}
	if pl["source"] != "pg_hardstorage@db1" {
		t.Errorf("payload.source = %v", pl["source"])
	}
}

func TestPagerDuty_Emit_FiltersBelowMinSeverity(t *testing.T) {
	var got atomic.Pointer[captured]
	srv := newServer(t, 202, &got)
	defer srv.Close()

	restore := pagerduty.OverrideEventsAPIv2URL(srv.URL)
	defer restore()

	s, _ := pagerduty.NewFromSpec(output.SinkSpec{
		Name:   "p",
		Plugin: "pagerduty",
		Config: map[string]any{
			"routing_key":  "abc",
			"min_severity": "error",
		},
	})
	defer s.Close()

	if err := s.Emit(context.Background(),
		output.NewEvent(output.SeverityWarning, "x", "y")); err != nil {
		t.Fatal(err)
	}
	if got.Load() != nil {
		t.Error("warning-severity event was emitted; should have been dropped")
	}
}
