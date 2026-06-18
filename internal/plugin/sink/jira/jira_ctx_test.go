package jira_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/sink/jira"
)

// External-review-pass-4: pre-Emit ctx.Err() check. The dedupe-by-
// subject path makes two HTTP calls (search + create-or-comment);
// an already-cancelled ctx must bail before the first.
func TestJira_PreCancelledCtx_RefusesEmit(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"issues":[]}`))
	}))
	defer srv.Close()

	s, err := jira.NewFromSpec(output.SinkSpec{
		Name: "x", Plugin: "jira", Config: map[string]any{
			"base_url":     srv.URL,
			"project":      "OPS",
			"email":        "ops@acme",
			"api_token":    "tok",
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
	if hits.Load() != 0 {
		t.Errorf("server should NOT have been hit; got %d requests", hits.Load())
	}
}
