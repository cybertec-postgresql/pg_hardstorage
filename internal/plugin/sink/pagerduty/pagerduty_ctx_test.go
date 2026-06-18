package pagerduty_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/sink/pagerduty"
)

// External-review-pass-4: pre-Emit ctx.Err() check matches the
// pattern established for the sibling sinks. Already-cancelled ctx
// must bail BEFORE we open the TCP connection to PD's events API.
func TestPagerDuty_PreCancelledCtx_RefusesEmit(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	restore := pagerduty.OverrideEventsAPIv2URL(srv.URL)
	defer restore()

	s, err := pagerduty.NewFromSpec(output.SinkSpec{
		Name: "x", Plugin: "pagerduty", Config: map[string]any{
			"routing_key":  "test",
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
