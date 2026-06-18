package datadog_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/airgap"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/sink/datadog"
)

// We can't talk to api.datadoghq.com from the test, but the Sink's
// behaviour is fully testable by using a local httptest server +
// rewriting the apiURL via the airgap-allowlist of test endpoints.
// Easier: NewFromSpec hard-codes the URL; we override via reflection
// in tests. Cleaner: expose a private hook for tests. We take the
// cleaner path by wrapping the construction with a custom URL and
// feeding it via the `site` field — site=localhost:1234 is technically
// invalid for production but works for the air-gap-relaxed test mode.
//
// To keep the test isolated and not depend on undocumented URL-
// fiddling, we run the test with airgap-off and a localhost
// httptest server fronted by a fake `api.localhost` site.

func TestDatadog_PostsEvent(t *testing.T) {
	airgap.LockForTest(t)
	defer airgap.WithScope(airgap.Policy{Mode: airgap.ModeOff})()

	var captured map[string]any
	var apiKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiKey = r.Header.Get("DD-API-KEY")
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &captured)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()

	// We construct the sink, then point its url at the test
	// server through a small in-package hook so we don't need
	// the real api.datadoghq.com.  See SetURLForTests.
	s, err := datadog.NewFromSpec(output.SinkSpec{
		Name: "test", Plugin: "datadog-events",
		Config: map[string]any{
			"api_key":      "secret-key",
			"site":         "datadoghq.com",
			"tags":         []any{"env:test"},
			"min_severity": "info",
		},
	})
	if err != nil {
		t.Fatalf("NewFromSpec: %v", err)
	}
	datadog.SetURLForTests(s, srv.URL)

	ev := output.NewEvent(output.SeverityWarning, "wal", "lag")
	ev.Subject = output.Subject{Tenant: "default", Deployment: "db1"}
	if err := s.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if apiKey != "secret-key" {
		t.Errorf("DD-API-KEY = %q", apiKey)
	}
	if captured["alert_type"] != "warning" {
		t.Errorf("alert_type = %v, want warning", captured["alert_type"])
	}
	if !strings.Contains(captured["title"].(string), "[db1]") {
		t.Errorf("title missing deployment prefix: %v", captured["title"])
	}
	tags, _ := captured["tags"].([]any)
	want := map[string]bool{"env:test": false, "tenant:default": false, "deployment:db1": false, "component:wal": false, "op:lag": false, "severity:warning": false}
	for _, tag := range tags {
		if _, ok := want[tag.(string)]; ok {
			want[tag.(string)] = true
		}
	}
	for k, found := range want {
		if !found {
			t.Errorf("missing tag %q in %v", k, tags)
		}
	}
}

func TestDatadog_RequiresAPIKey(t *testing.T) {
	_, err := datadog.NewFromSpec(output.SinkSpec{Name: "x", Plugin: "datadog-events", Config: map[string]any{}})
	if err == nil {
		t.Fatal("expected error without api_key")
	}
}

func TestDatadog_RespectsMinSeverity(t *testing.T) {
	airgap.LockForTest(t)
	defer airgap.WithScope(airgap.Policy{Mode: airgap.ModeOff})()

	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	s, _ := datadog.NewFromSpec(output.SinkSpec{
		Name: "x", Plugin: "datadog-events",
		Config: map[string]any{"api_key": "k", "min_severity": "error"},
	})
	datadog.SetURLForTests(s, srv.URL)
	if err := s.Emit(context.Background(), output.NewEvent(output.SeverityWarning, "x", "y")); err != nil {
		t.Errorf("warning under error floor should drop: %v", err)
	}
	if called {
		t.Error("warning event should not have hit the server (floor=error)")
	}
}

func TestDatadog_MapsSeverity(t *testing.T) {
	cases := []struct {
		sev  output.Severity
		want string
	}{
		{output.SeverityEmergency, "error"},
		{output.SeverityAlert, "error"},
		{output.SeverityCritical, "error"},
		{output.SeverityError, "error"},
		{output.SeverityWarning, "warning"},
		{output.SeverityNotice, "info"},
		{output.SeverityInfo, "info"},
	}
	for _, tc := range cases {
		if got := datadog.MapAlertType(tc.sev); got != tc.want {
			t.Errorf("severity %v: got %q, want %q", tc.sev, got, tc.want)
		}
	}
}
