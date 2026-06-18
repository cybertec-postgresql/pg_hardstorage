package webhook_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/sink/webhook"
)

// External-review-pass-4: pre-Emit ctx.Err() check matches the
// pattern established for the sibling sinks. Already-cancelled ctx
// must bail BEFORE we open the TCP connection.
func TestWebhook_PreCancelledCtx_RefusesEmit(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s, err := webhook.NewFromSpec(output.SinkSpec{
		Name: "x", Plugin: "webhook", Config: map[string]any{
			"url":          srv.URL,
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
