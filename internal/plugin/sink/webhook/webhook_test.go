package webhook_test

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
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/sink/webhook"
)

type captured struct {
	method, contentType, auth string
	body                      []byte
}

func newServer(t *testing.T, status int, store *atomic.Pointer[captured]) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		store.Store(&captured{
			method:      r.Method,
			contentType: r.Header.Get("Content-Type"),
			auth:        r.Header.Get("Authorization"),
			body:        body,
		})
		w.WriteHeader(status)
	}))
}

func TestWebhook_PostsEventJSON(t *testing.T) {
	var got atomic.Pointer[captured]
	srv := newServer(t, 204, &got)
	defer srv.Close()

	s, err := webhook.NewFromSpec(output.SinkSpec{Name: "w", Plugin: "webhook", Config: map[string]any{
		"url":          srv.URL,
		"auth_header":  "Bearer token",
		"min_severity": "info",
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ev := output.NewEvent(output.SeverityWarning, "wal.stream", "starting")
	if err := s.Emit(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	c := got.Load()
	if c == nil {
		t.Fatal("server didn't receive a request")
	}
	if c.method != "POST" {
		t.Errorf("method = %q, want POST", c.method)
	}
	if c.contentType != "application/json" {
		t.Errorf("Content-Type = %q", c.contentType)
	}
	if c.auth != "Bearer token" {
		t.Errorf("Authorization = %q", c.auth)
	}
	var ev2 output.Event
	if err := json.Unmarshal(c.body, &ev2); err != nil {
		t.Fatalf("body not valid Event JSON: %v\n%s", err, c.body)
	}
	if ev2.Op != "starting" {
		t.Errorf("body Op = %q", ev2.Op)
	}
}

func TestWebhook_FiltersBelowMinSeverity(t *testing.T) {
	var got atomic.Pointer[captured]
	srv := newServer(t, 200, &got)
	defer srv.Close()

	s, err := webhook.NewFromSpec(output.SinkSpec{Name: "w", Plugin: "webhook", Config: map[string]any{
		"url":          srv.URL,
		"min_severity": "error",
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Emit(context.Background(), output.NewEvent(output.SeverityWarning, "x", "y")); err != nil {
		t.Fatal(err)
	}
	if got.Load() != nil {
		t.Error("warning-severity dropped below error threshold should not POST")
	}
}

func TestWebhook_RejectsBadMethod(t *testing.T) {
	_, err := webhook.NewFromSpec(output.SinkSpec{Name: "w", Plugin: "webhook", Config: map[string]any{
		"url":    "http://x",
		"method": "DELETE",
	}})
	if err == nil || !strings.Contains(err.Error(), "DELETE") {
		t.Errorf("expected method-rejected error; got %v", err)
	}
}

func TestWebhook_RequiresURL(t *testing.T) {
	_, err := webhook.NewFromSpec(output.SinkSpec{Name: "w", Plugin: "webhook"})
	if err == nil || !strings.Contains(err.Error(), "url") {
		t.Errorf("expected url-required error; got %v", err)
	}
}
