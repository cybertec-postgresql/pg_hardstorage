// metrics_listener.go — the agent's optional Prometheus /metrics
// listener.
//
// The control plane always serves /metrics from its REST listener, but
// the backup / WAL-archive / verify pipelines run inside the AGENT
// process, so their counters live in the agent's memory.  To make those
// scrapable we let the agent bind a small, dedicated HTTP server that
// serves only GET /metrics from the process-wide registry.
//
// It is opt-in (empty --metrics-listen disables it) and defaults to a
// loopback bind so an operator who turns it on doesn't accidentally
// expose the surface on every interface.  Per the project's
// no-machine-specific-defaults rule, the chosen address is announced at
// launch through the dispatcher's event stream.
package cli

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/obs/metrics"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/version"
)

// startMetricsListener binds a /metrics HTTP server on addr and serves
// until ctx is cancelled.  A nil/empty addr is a no-op (returns a no-op
// stop func), so callers can wire it unconditionally.  Bind failures are
// surfaced as a warning event and downgraded to "metrics disabled" — a
// busy port must not stop the agent from doing its actual job.
//
// Returns a stop func the caller defers; it triggers a graceful
// shutdown and is safe to call even when the listener never started.
func startMetricsListener(ctx context.Context, d *output.Dispatcher, addr string) func() {
	if addr == "" {
		return func() {}
	}

	// Publish build info so the endpoint always carries a real sample.
	metrics.SetBuildInfo(version.Version, version.Commit)

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		_ = d.Event(ctx, output.NewEvent(output.SeverityWarning, "metrics", "listen_failed").
			WithBody(map[string]any{"address": addr, "error": err.Error()}))
		return func() {}
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", metrics.Default().Handler())
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	_ = d.Event(ctx, output.NewEvent(output.SeverityNotice, "metrics", "listening").
		WithBody(map[string]any{"address": ln.Addr().String()}))

	go func() {
		if serveErr := srv.Serve(ln); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			_ = d.Event(ctx, output.NewEvent(output.SeverityWarning, "metrics", "serve_failed").
				WithBody(map[string]any{"error": serveErr.Error()}))
		}
	}()

	return func() {
		shCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shCtx)
	}
}
