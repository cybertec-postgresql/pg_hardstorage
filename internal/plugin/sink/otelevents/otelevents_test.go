package otelevents_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/sink/otelevents"
)

func TestOTel_PostsLogsPayload(t *testing.T) {
	var captured map[string]any
	var contentType string
	var customHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentType = r.Header.Get("Content-Type")
		customHeader = r.Header.Get("X-Honeycomb-Team")
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &captured)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"partialSuccess":{}}`))
	}))
	defer srv.Close()

	s, err := otelevents.NewFromSpec(output.SinkSpec{
		Name: "test", Plugin: "otel-events",
		Config: map[string]any{
			"endpoint": srv.URL,
			"headers": map[string]any{
				"x-honeycomb-team": "secret-team-key",
			},
			"min_severity": "info",
			"service_name": "pg_hardstorage_test",
		},
	})
	if err != nil {
		t.Fatalf("NewFromSpec: %v", err)
	}
	ev := output.NewEvent(output.SeverityWarning, "wal", "lag")
	ev.Subject = output.Subject{Deployment: "db1", Tenant: "default", BackupID: "db1.full.x"}
	if err := s.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if contentType != "application/json" {
		t.Errorf("Content-Type = %q", contentType)
	}
	if customHeader != "secret-team-key" {
		t.Errorf("custom header = %q", customHeader)
	}
	resourceLogs, ok := captured["resourceLogs"].([]any)
	if !ok || len(resourceLogs) != 1 {
		t.Fatalf("expected 1 resourceLogs entry: %#v", captured)
	}
	rl := resourceLogs[0].(map[string]any)
	scopeLogs := rl["scopeLogs"].([]any)
	logRecord := scopeLogs[0].(map[string]any)["logRecords"].([]any)[0].(map[string]any)
	if logRecord["severityText"] != "warning" {
		t.Errorf("severityText = %v", logRecord["severityText"])
	}
	if int(logRecord["severityNumber"].(float64)) != 13 {
		t.Errorf("severityNumber = %v, want 13 (WARN)", logRecord["severityNumber"])
	}
	attrs, _ := logRecord["attributes"].([]any)
	wantAttrs := map[string]string{
		"deployment": "db1",
		"tenant":     "default",
		"component":  "wal",
		"op":         "lag",
		"backup_id":  "db1.full.x",
	}
	got := map[string]string{}
	for _, a := range attrs {
		am := a.(map[string]any)
		key := am["key"].(string)
		val := am["value"].(map[string]any)["stringValue"].(string)
		got[key] = val
	}
	for k, v := range wantAttrs {
		if got[k] != v {
			t.Errorf("attribute %s = %q, want %q", k, got[k], v)
		}
	}
}

func TestOTel_AppendsLogsPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/logs" {
			t.Errorf("expected /v1/logs, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	s, _ := otelevents.NewFromSpec(output.SinkSpec{
		Name: "x", Plugin: "otel-events",
		Config: map[string]any{"endpoint": srv.URL, "min_severity": "info"},
	})
	s.Emit(context.Background(), output.NewEvent(output.SeverityWarning, "x", "y"))
}

func TestOTel_RequiresEndpoint(t *testing.T) {
	_, err := otelevents.NewFromSpec(output.SinkSpec{Name: "x", Plugin: "otel-events", Config: map[string]any{}})
	if err == nil {
		t.Fatal("expected error without endpoint")
	}
}

func TestOTel_RespectsMinSeverity(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	s, _ := otelevents.NewFromSpec(output.SinkSpec{
		Name: "x", Plugin: "otel-events",
		Config: map[string]any{"endpoint": srv.URL, "min_severity": "error"},
	})
	if err := s.Emit(context.Background(), output.NewEvent(output.SeverityWarning, "x", "y")); err != nil {
		t.Errorf("warning under error floor should drop: %v", err)
	}
	if called {
		t.Error("warning event should not have hit the server (floor=error)")
	}
}

func TestOTel_StatusErrorBubbles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"code":"401","message":"unauthorized"}`))
	}))
	defer srv.Close()
	s, _ := otelevents.NewFromSpec(output.SinkSpec{
		Name: "x", Plugin: "otel-events",
		Config: map[string]any{"endpoint": srv.URL, "min_severity": "info"},
	})
	err := s.Emit(context.Background(), output.NewEvent(output.SeverityWarning, "x", "y"))
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("expected 401 error, got %v", err)
	}
}
