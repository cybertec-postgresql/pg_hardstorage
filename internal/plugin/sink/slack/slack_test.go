package slack_test

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
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/sink/slack"
)

func newServer(t *testing.T, status int, capture *atomic.Pointer[string]) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s := string(body)
		capture.Store(&s)
		w.WriteHeader(status)
		_, _ = w.Write([]byte("ok"))
	}))
}

func mustBuild(t *testing.T, cfg map[string]any) output.Sink {
	t.Helper()
	s, err := slack.NewFromSpec(output.SinkSpec{Name: "test", Plugin: "slack", Config: cfg})
	if err != nil {
		t.Fatalf("NewFromSpec: %v", err)
	}
	return s
}

func TestSlack_PostsExpectedShape(t *testing.T) {
	var captured atomic.Pointer[string]
	srv := newServer(t, 200, &captured)
	defer srv.Close()

	s := mustBuild(t, map[string]any{
		"webhook_url":  srv.URL,
		"channel":      "#ops",
		"min_severity": "info",
	})
	defer s.Close()

	ev := output.NewEvent(output.SeverityWarning, "backup", "manifest.replica_failed").
		WithSubject(output.Subject{Deployment: "db1", BackupID: "db1.full.20260428T1200Z"}).
		WithBody(map[string]any{"error": "disk full"}).
		WithSuggestion(&output.Suggestion{Human: "free space and retry", Command: "pg_hardstorage doctor db1"})

	if err := s.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	bodyPtr := captured.Load()
	if bodyPtr == nil {
		t.Fatal("server never received a request")
	}
	var p struct {
		Channel string `json:"channel"`
		Text    string `json:"text"`
		Blocks  []any  `json:"blocks"`
	}
	if err := json.Unmarshal([]byte(*bodyPtr), &p); err != nil {
		t.Fatalf("unmarshal posted body: %v\n%s", err, *bodyPtr)
	}
	if p.Channel != "#ops" {
		t.Errorf("channel = %q, want #ops", p.Channel)
	}
	for _, want := range []string{"WARNING", "manifest.replica_failed", "deployment=db1"} {
		if !strings.Contains(p.Text, want) {
			t.Errorf("text missing %q; got %q", want, p.Text)
		}
	}
	if len(p.Blocks) < 2 {
		t.Errorf("expected at least 2 blocks (header + body); got %d", len(p.Blocks))
	}
}

func TestSlack_FiltersBelowMinSeverity(t *testing.T) {
	var captured atomic.Pointer[string]
	srv := newServer(t, 200, &captured)
	defer srv.Close()

	s := mustBuild(t, map[string]any{
		"webhook_url":  srv.URL,
		"min_severity": "warning",
	})
	defer s.Close()

	// Info is less severe than warning; must be dropped.
	ev := output.NewEvent(output.SeverityInfo, "backup", "started")
	if err := s.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if captured.Load() != nil {
		t.Errorf("info-severity event was sent; should have been dropped")
	}

	// Warning equals threshold — must be sent.
	ev = output.NewEvent(output.SeverityWarning, "backup", "wal.gap_detected")
	if err := s.Emit(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if captured.Load() == nil {
		t.Error("warning-severity event was NOT sent")
	}
}

// TestSlack_SeverityFilter_PinsRFC5424Direction is a paranoia test
// against a class of bug a reviewer flagged: "RFC 5424 lower=more
// severe; is the comparison flipped?" The threshold cases below
// pin the correct semantics so a future refactor can't silently
// invert them.
func TestSlack_SeverityFilter_PinsRFC5424Direction(t *testing.T) {
	cases := []struct {
		name       string
		minSev     string
		event      output.Severity
		shouldEmit bool
	}{
		// Threshold = warning (4)
		{"emergency<warning passes", "warning", output.SeverityEmergency, true},
		{"alert<warning passes", "warning", output.SeverityAlert, true},
		{"critical<warning passes", "warning", output.SeverityCritical, true},
		{"error<warning passes", "warning", output.SeverityError, true},
		{"warning=warning passes", "warning", output.SeverityWarning, true},
		{"notice>warning drops", "warning", output.SeverityNotice, false},
		{"info>warning drops", "warning", output.SeverityInfo, false},
		{"debug>warning drops", "warning", output.SeverityDebug, false},

		// Threshold = info (6) — almost everything passes.
		{"warning<info passes", "info", output.SeverityWarning, true},
		{"info=info passes", "info", output.SeverityInfo, true},
		{"debug>info drops", "info", output.SeverityDebug, false},

		// Threshold = critical (2) — only the most severe pass.
		{"emergency<critical passes", "critical", output.SeverityEmergency, true},
		{"critical=critical passes", "critical", output.SeverityCritical, true},
		{"error>critical drops", "critical", output.SeverityError, false},
		{"warning>critical drops", "critical", output.SeverityWarning, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var captured atomic.Pointer[string]
			srv := newServer(t, 200, &captured)
			defer srv.Close()

			s := mustBuild(t, map[string]any{
				"webhook_url":  srv.URL,
				"min_severity": c.minSev,
			})
			defer s.Close()

			ev := output.NewEvent(c.event, "test", "op")
			if err := s.Emit(context.Background(), ev); err != nil {
				t.Fatalf("Emit: %v", err)
			}
			emitted := captured.Load() != nil
			if emitted != c.shouldEmit {
				t.Errorf("event=%s threshold=%s: emitted=%v, want %v",
					c.event, c.minSev, emitted, c.shouldEmit)
			}
		})
	}
}

func TestSlack_PropagatesNon2xxAsError(t *testing.T) {
	var captured atomic.Pointer[string]
	srv := newServer(t, 500, &captured)
	defer srv.Close()

	s := mustBuild(t, map[string]any{"webhook_url": srv.URL})
	defer s.Close()

	err := s.Emit(context.Background(), output.NewEvent(output.SeverityError, "x", "y"))
	if err == nil {
		t.Fatal("expected non-2xx error")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention status code; got %v", err)
	}
}

func TestSlack_RequiresWebhookURL(t *testing.T) {
	_, err := slack.NewFromSpec(output.SinkSpec{Name: "x", Plugin: "slack", Config: map[string]any{}})
	if err == nil || !strings.Contains(err.Error(), "webhook_url") {
		t.Errorf("expected webhook_url required error; got %v", err)
	}
}

func TestSlack_EmitAfterCloseFails(t *testing.T) {
	var captured atomic.Pointer[string]
	srv := newServer(t, 200, &captured)
	defer srv.Close()

	s := mustBuild(t, map[string]any{"webhook_url": srv.URL})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Emit(context.Background(), output.NewEvent(output.SeverityError, "x", "y")); err == nil {
		t.Error("Emit after Close should fail")
	}
}

func TestSlack_RegistersWithDefaultRegistry(t *testing.T) {
	plugins := output.DefaultSinkRegistry.Plugins()
	found := false
	for _, p := range plugins {
		if p == "slack" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("slack should self-register; default registry plugins = %v", plugins)
	}
}
