package server_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/server"
)

// TestHTTPMetrics_MethodLabelBounded pins M40: the method label came
// straight from the request line, so an unauthenticated client sending
// arbitrary methods minted one Prometheus series per method, forever.
// Anything outside the standard set folds to "other".
func TestHTTPMetrics_MethodLabelBounded(t *testing.T) {
	s, err := server.New(server.Config{Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(s.Handler())
	defer hs.Close()
	for i := 0; i < 5; i++ {
		req, _ := http.NewRequest(fmt.Sprintf("ZZMETHOD%d", i), hs.URL+"/v1/healthz", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	_, body := get(t, hs.URL, "/metrics")
	if strings.Contains(body, "ZZMETHOD") {
		t.Fatalf("arbitrary request methods became metric label values:\n%s", linesWith(body, "ZZMETHOD"))
	}
	if !strings.Contains(body, `method="other"`) {
		t.Errorf(`want non-standard methods folded to method="other"`)
	}
}

func linesWith(s, sub string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, sub) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// listCountingBackend counts full List walks.
type listCountingBackend struct {
	*server.MemoryBackend
	lists atomic.Int32
}

func (b *listCountingBackend) List(ctx context.Context, opts server.ListOptions) ([]server.Job, error) {
	b.lists.Add(1)
	return b.MemoryBackend.List(ctx, opts)
}

// TestMetrics_ScrapeDoesNotListJobs pins M41: every unauthenticated
// /metrics scrape ran an unbounded List of every job -- with progress
// arrays -- just to count them by state. The census is now an aggregate
// count (GROUP BY state on the PG backend).
func TestMetrics_ScrapeDoesNotListJobs(t *testing.T) {
	b := &listCountingBackend{MemoryBackend: server.NewMemoryBackend()}
	s, err := server.NewWithJobs(server.Config{Listen: "127.0.0.1:0"}, server.NewJobRegistryWithBackend(b))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Jobs().Enqueue(server.EnqueueOptions{Kind: server.JobBackup, Deployment: "db1"}); err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(s.Handler())
	defer hs.Close()
	for i := 0; i < 20; i++ {
		get(t, hs.URL, "/metrics")
	}
	if n := b.lists.Load(); n != 0 {
		t.Errorf("/metrics ran %d full job List walks over 20 scrapes; want 0 (aggregate census)", n)
	}
	_, body := get(t, hs.URL, "/metrics")
	if !strings.Contains(body, `state="queued"} 1`) {
		t.Errorf("census lost the queued job:\n%s", linesWith(body, "state="))
	}
}
