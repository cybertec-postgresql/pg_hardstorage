// metrics.go — the control plane's Prometheus surface.
//
// Two pieces live here:
//
//   - handleMetrics serves the process-wide metric registry at GET
//     /metrics in Prometheus exposition format.  It refreshes the
//     scrape-time gauges (job counts, agent liveness, repo count) from
//     THIS server instance immediately before rendering, so the numbers
//     reflect the control plane that was actually scraped rather than
//     whichever server registered a global collector last.  That keeps
//     the endpoint correct even when several servers share the process
//     (the test suite stands up many).
//
//   - withHTTPMetrics wraps the route mux so every served request bumps
//     pg_hardstorage_http_requests_total and observes its latency,
//     bucketed by a low-cardinality route label (never the raw path —
//     job IDs and deployment names would explode the series count).
package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/obs/metrics"
)

// handleMetrics renders the metric registry.  Unauthenticated, like the
// health probes: a Prometheus scraper shouldn't need the operator bearer
// token, and the endpoint exposes only aggregate counters — no secrets,
// no per-object data.  Operators who want it locked down put it behind
// the same mTLS the rest of the listener uses, or scrape over a private
// network interface.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	s.refreshScrapeGauges()
	metrics.Default().Handler().ServeHTTP(w, r)
}

// refreshScrapeGauges samples this server's in-memory state into the
// scrape-time gauges.  Cheap: the job list is an in-memory walk and the
// agent registry is a small map.  Called once per scrape.
func (s *Server) refreshScrapeGauges() {
	// Seed every known state at zero so an idle control plane still
	// emits the full series set (a dashboard panel for "failed jobs"
	// shouldn't read as "no data" just because nothing has failed yet).
	counts := map[string]int{
		string(JobQueued):    0,
		string(JobRunning):   0,
		string(JobCompleted): 0,
		string(JobFailed):    0,
		string(JobCancelled): 0,
	}
	for _, j := range s.jobs.List(ListOptions{}) {
		counts[string(j.State)]++
	}
	metrics.SetJobsByState(counts)

	total := len(s.agents.List(true))
	active := len(s.agents.List(false))
	metrics.SetAgents(active, total)

	metrics.SetReposConfigured(len(s.cfg.Repos))
}

// statusRecorder captures the response status code so the HTTP-request
// metric can label by code.  Defaults to 200 — handlers that write a
// body without calling WriteHeader implicitly return 200.
type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.code = code
	sr.ResponseWriter.WriteHeader(code)
}

// withHTTPMetrics wraps next so every request is counted + timed.
func withHTTPMetrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sr := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		route := routeLabel(r.URL.Path)
		next.ServeHTTP(sr, r)
		metrics.HTTPRequest(route, r.Method, codeLabel(sr.code), time.Since(start).Seconds())
	})
}

// routeLabel folds a request path to a bounded set of route labels.  We
// never label on the raw path: /v1/jobs/<id>/progress would otherwise
// mint a new series per job.
func routeLabel(path string) string {
	switch {
	case path == "/metrics":
		return "metrics"
	case path == "/v1/healthz":
		return "healthz"
	case path == "/v1/readyz":
		return "readyz"
	case path == "/v1/version":
		return "version"
	case strings.HasPrefix(path, "/v1/deployments"):
		return "deployments"
	case strings.HasPrefix(path, "/v1/agents"):
		return "agents"
	case strings.HasPrefix(path, "/v1/jobs"):
		return "jobs"
	default:
		return "other"
	}
}

// codeLabel renders an HTTP status code as a string label.
func codeLabel(code int) string {
	if code == 0 {
		code = http.StatusOK
	}
	return strconv.Itoa(code)
}
