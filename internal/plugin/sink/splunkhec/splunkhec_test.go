package splunkhec_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/sink/splunkhec"
)

func TestSplunkHEC_PostsEvent(t *testing.T) {
	var captured map[string]any
	var token string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &captured)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"text":"Success","code":0}`))
	}))
	defer srv.Close()

	s, err := splunkhec.NewFromSpec(output.SinkSpec{
		Name: "test", Plugin: "splunk-hec",
		Config: map[string]any{
			"url":          srv.URL,
			"token":        "secret-hec-token",
			"index":        "pg_hardstorage",
			"source":       "test",
			"min_severity": "info",
		},
	})
	if err != nil {
		t.Fatalf("NewFromSpec: %v", err)
	}
	ev := output.NewEvent(output.SeverityWarning, "wal", "lag")
	if err := s.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if token != "Splunk secret-hec-token" {
		t.Errorf("auth header = %q, want Splunk secret-hec-token", token)
	}
	if captured["index"] != "pg_hardstorage" {
		t.Errorf("index missing: %#v", captured)
	}
	if captured["source"] != "test" {
		t.Errorf("source missing: %#v", captured)
	}
	if _, ok := captured["event"]; !ok {
		t.Errorf("event payload missing: %#v", captured)
	}
}

func TestSplunkHEC_RequiresURLAndToken(t *testing.T) {
	cases := []map[string]any{
		{},                               // missing url
		{"url": "http://localhost:8088"}, // missing token
	}
	for i, cfg := range cases {
		_, err := splunkhec.NewFromSpec(output.SinkSpec{Name: "x", Plugin: "splunk-hec", Config: cfg})
		if err == nil {
			t.Errorf("case %d: expected error, got nil", i)
		}
	}
}

func TestSplunkHEC_RespectsMinSeverity(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	s, _ := splunkhec.NewFromSpec(output.SinkSpec{
		Name: "x", Plugin: "splunk-hec",
		Config: map[string]any{
			"url": srv.URL, "token": "t", "min_severity": "warning",
		},
	})
	if err := s.Emit(context.Background(), output.NewEvent(output.SeverityInfo, "x", "y")); err != nil {
		t.Errorf("info should drop silently: %v", err)
	}
	if called {
		t.Error("info-level event should not have hit the server")
	}
}

func TestSplunkHEC_StatusErrorBubbles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"text":"Token disabled","code":1}`))
	}))
	defer srv.Close()
	s, _ := splunkhec.NewFromSpec(output.SinkSpec{
		Name: "x", Plugin: "splunk-hec",
		Config: map[string]any{"url": srv.URL, "token": "t", "min_severity": "info"},
	})
	err := s.Emit(context.Background(), output.NewEvent(output.SeverityWarning, "x", "y"))
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Errorf("expected 400 error, got %v", err)
	}
}
