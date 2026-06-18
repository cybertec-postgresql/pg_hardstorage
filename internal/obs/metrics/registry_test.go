package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// scrape renders r to a string for assertion.
func scrape(t *testing.T, r *Registry) string {
	t.Helper()
	var sb strings.Builder
	if err := r.WriteExposition(&sb); err != nil {
		t.Fatalf("WriteExposition: %v", err)
	}
	return sb.String()
}

// mustContain fails unless every wanted line appears in body.
func mustContain(t *testing.T, body string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(body, w) {
			t.Errorf("exposition missing %q\n--- full body ---\n%s", w, body)
		}
	}
}

func TestCounterExposition(t *testing.T) {
	r := NewRegistry()
	c := r.RegisterCounter("test_requests_total", "Requests handled.", "route", "code")
	c.With("backups", "200").Inc()
	c.With("backups", "200").Inc()
	c.With("backups", "500").Add(3)

	body := scrape(t, r)
	mustContain(t, body,
		"# HELP test_requests_total Requests handled.",
		"# TYPE test_requests_total counter",
		`test_requests_total{route="backups",code="200"} 2`,
		`test_requests_total{route="backups",code="500"} 3`,
	)
}

func TestGaugeSetAddIncDec(t *testing.T) {
	r := NewRegistry()
	g := r.RegisterGauge("test_queue_depth", "Queue depth.", "queue")
	g.With("main").Set(10)
	g.With("main").Dec()
	g.With("main").Add(2.5)

	body := scrape(t, r)
	mustContain(t, body, `test_queue_depth{queue="main"} 11.5`)
}

func TestUnlabelledGauge(t *testing.T) {
	r := NewRegistry()
	g := r.RegisterGauge("test_build_info", "Build.")
	g.With().Set(1)

	body := scrape(t, r)
	// No labels → no braces.
	mustContain(t, body, "test_build_info 1")
	if strings.Contains(body, "test_build_info{") {
		t.Errorf("unlabelled gauge should not render braces:\n%s", body)
	}
}

func TestHistogramCumulativeBuckets(t *testing.T) {
	r := NewRegistry()
	h := r.RegisterHistogram("test_latency_seconds", "Latency.",
		[]float64{0.1, 0.5, 1}, "op")
	// Observations: 0.05 (→0.1), 0.2 (→0.5), 0.2 (→0.5), 2 (→+Inf only).
	h.With("unwrap").Observe(0.05)
	h.With("unwrap").Observe(0.2)
	h.With("unwrap").Observe(0.2)
	h.With("unwrap").Observe(2)

	body := scrape(t, r)
	mustContain(t, body,
		"# TYPE test_latency_seconds histogram",
		`test_latency_seconds_bucket{op="unwrap",le="0.1"} 1`,
		`test_latency_seconds_bucket{op="unwrap",le="0.5"} 3`, // cumulative: 1 + 2
		`test_latency_seconds_bucket{op="unwrap",le="1"} 3`,   // nothing between 0.5 and 1
		`test_latency_seconds_bucket{op="unwrap",le="+Inf"} 4`,
		`test_latency_seconds_count{op="unwrap"} 4`,
	)
	// Sum = 0.05 + 0.2 + 0.2 + 2 = 2.45
	mustContain(t, body, `test_latency_seconds_sum{op="unwrap"} 2.45`)
}

func TestLabelValueEscaping(t *testing.T) {
	r := NewRegistry()
	c := r.RegisterCounter("test_escaped_total", "Escaping.", "msg")
	c.With(`a"b\c` + "\n" + "d").Inc()

	body := scrape(t, r)
	mustContain(t, body, `test_escaped_total{msg="a\"b\\c\nd"} 1`)
}

func TestEmptyFamilyStillAdvertisesSchema(t *testing.T) {
	r := NewRegistry()
	r.RegisterCounter("test_never_observed_total", "Idle.", "x")

	body := scrape(t, r)
	mustContain(t, body,
		"# HELP test_never_observed_total Idle.",
		"# TYPE test_never_observed_total counter",
	)
}

func TestDeterministicSeriesOrder(t *testing.T) {
	r := NewRegistry()
	c := r.RegisterCounter("test_ordered_total", "Order.", "k")
	// Insert out of lexical order; render must sort.
	c.With("zebra").Inc()
	c.With("alpha").Inc()
	c.With("mike").Inc()

	body := scrape(t, r)
	ia := strings.Index(body, `k="alpha"`)
	im := strings.Index(body, `k="mike"`)
	iz := strings.Index(body, `k="zebra"`)
	if !(ia < im && im < iz) {
		t.Errorf("series not in sorted order: alpha=%d mike=%d zebra=%d\n%s", ia, im, iz, body)
	}
}

func TestCollectorRunsBeforeRender(t *testing.T) {
	r := NewRegistry()
	g := r.RegisterGauge("test_collected", "Collected at scrape.")
	calls := 0
	r.AddCollector(func() {
		calls++
		g.With().Set(float64(calls))
	})

	_ = scrape(t, r)
	body := scrape(t, r)
	// Two scrapes → collector ran twice → value 2.
	mustContain(t, body, "test_collected 2")
}

func TestReRegisterReturnsSameFamily(t *testing.T) {
	r := NewRegistry()
	a := r.RegisterCounter("test_dup_total", "Dup.", "l")
	b := r.RegisterCounter("test_dup_total", "Dup.", "l")
	a.With("x").Inc()
	b.With("x").Inc()

	body := scrape(t, r)
	// Both handles point at one family/series → value 2, single line.
	mustContain(t, body, `test_dup_total{l="x"} 2`)
	if n := strings.Count(body, "# TYPE test_dup_total"); n != 1 {
		t.Errorf("family declared %d times, want 1", n)
	}
}

func TestHandlerServesExposition(t *testing.T) {
	r := NewRegistry()
	r.RegisterGauge("test_handler_up", "Up.").With().Set(1)

	srv := httptest.NewServer(r.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content-type = %q, want text/plain…", ct)
	}

	// Non-GET is rejected.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/metrics", nil)
	pr, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer pr.Body.Close()
	if pr.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d, want 405", pr.StatusCode)
	}
}

func TestConcurrentIncrements(t *testing.T) {
	r := NewRegistry()
	c := r.RegisterCounter("test_concurrent_total", "Race.", "l")
	const goroutines, perG = 16, 1000
	done := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		go func() {
			for j := 0; j < perG; j++ {
				c.With("a").Inc()
			}
			done <- struct{}{}
		}()
	}
	for i := 0; i < goroutines; i++ {
		<-done
	}
	body := scrape(t, r)
	mustContain(t, body, `test_concurrent_total{l="a"} 16000`)
}
