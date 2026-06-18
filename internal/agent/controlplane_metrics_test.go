package agent_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/agent"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/obs/metrics"
)

// metricValue scrapes the default registry and returns the value of the
// first exposition line containing substr (0 if absent).
func metricValue(t *testing.T, substr string) float64 {
	t.Helper()
	var sb strings.Builder
	if err := metrics.Default().WriteExposition(&sb); err != nil {
		t.Fatalf("WriteExposition: %v", err)
	}
	for _, ln := range strings.Split(sb.String(), "\n") {
		if strings.HasPrefix(ln, "#") || !strings.Contains(ln, substr) {
			continue
		}
		f := strings.Fields(ln)
		if len(f) >= 2 {
			v, _ := strconv.ParseFloat(f[len(f)-1], 64)
			return v
		}
	}
	return 0
}

// TestControlPlane_HeartbeatFailureIncrementsMetric pins observability
// audit #5: when an agent can't reach the control plane, the error
// increments controlplane_errors_total — not just stderr — so a fleet
// whose agents are failing is alertable, not silent.
func TestControlPlane_HeartbeatFailureIncrementsMetric(t *testing.T) {
	const line = `controlplane_errors_total{op="heartbeat"}`
	before := metricValue(t, line)

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/agents/heartbeat", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError) // every heartbeat fails
	})
	hs := httptest.NewServer(mux)
	defer hs.Close()

	c := &agent.ControlPlaneClient{
		BaseURL:           hs.URL,
		AgentID:           "metric-agent",
		Host:              "h",
		Version:           "t",
		HeartbeatInterval: 30 * time.Millisecond,
		PollInterval:      time.Hour, // don't poll during this short test
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_ = c.Run(ctx)

	if after := metricValue(t, line); after <= before {
		t.Fatalf("controlplane_errors_total{op=heartbeat} did not increment: before=%v after=%v", before, after)
	}
}
