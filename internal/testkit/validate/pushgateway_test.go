package validate_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/validate"
)

// pushgatewayServer captures every request the emitter makes
// so tests can assert on URL + body without a real Pushgateway.
type pushgatewayServer struct {
	mu       sync.Mutex
	requests []recordedRequest
	server   *httptest.Server
}

type recordedRequest struct {
	URL    string
	Method string
	Body   string
}

func newPushgatewayServer(t *testing.T) *pushgatewayServer {
	t.Helper()
	p := &pushgatewayServer{}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		p.mu.Lock()
		p.requests = append(p.requests, recordedRequest{
			URL: r.URL.String(), Method: r.Method, Body: string(body),
		})
		p.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(p.server.Close)
	return p
}

func (p *pushgatewayServer) calls() []recordedRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]recordedRequest{}, p.requests...)
}

func TestPushgatewayEmitter_DisabledOnEmptyURL(t *testing.T) {
	e := validate.NewPushgatewayEmitter("", "job", "inst")
	e.OnEvent(validate.Event{Cell: "c", Op: "iter_start", Iteration: 1})
	if err := e.Push(context.Background()); err != nil {
		t.Errorf("disabled emitter shouldn't error: %v", err)
	}
}

func TestPushgatewayEmitter_PushHasMetrics(t *testing.T) {
	srv := newPushgatewayServer(t)
	e := validate.NewPushgatewayEmitter(srv.server.URL,
		"pg_hardstorage_validate_test", "test-instance")
	e.OnEvent(validate.Event{Cell: "u24", Op: "iter_start", Iteration: 12,
		At: time.Now().UTC()})
	e.OnEvent(validate.Event{Cell: "u24", Op: "backup_started"})
	e.OnEvent(validate.Event{Cell: "u24", Op: "fault_apply"})
	e.AnnotateCellMetadata("u24", "ubuntu:24.04", "17")

	if err := e.Push(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls := srv.calls()
	if len(calls) != 1 {
		t.Fatalf("expected exactly one push; got %d", len(calls))
	}
	if calls[0].Method != http.MethodPut {
		t.Errorf("method: %s", calls[0].Method)
	}
	body := calls[0].Body
	for _, want := range []string{
		"pg_hardstorage_validate_iterations",
		"pg_hardstorage_validate_backups_total",
		"pg_hardstorage_validate_faults_applied_total",
		`cell="u24"`,
		`os="ubuntu:24.04"`,
		`pg="17"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n---\n%s", want, body)
		}
	}
}

func TestPushgatewayEmitter_FailureMarksPassZero(t *testing.T) {
	srv := newPushgatewayServer(t)
	e := validate.NewPushgatewayEmitter(srv.server.URL, "job", "inst")
	e.OnEvent(validate.Event{Cell: "x", Op: "verify_failed", Iteration: 1})
	if err := e.Push(context.Background()); err != nil {
		t.Fatal(err)
	}
	body := srv.calls()[0].Body
	if !strings.Contains(body, `pg_hardstorage_validate_pass{cell="x",os="",pg=""} 0`) {
		t.Errorf("expected pass=0 for failed cell; body=%s", body)
	}
}

func TestPushgatewayEmitter_RunFlushesOnContextCancel(t *testing.T) {
	srv := newPushgatewayServer(t)
	e := validate.NewPushgatewayEmitter(srv.server.URL, "job", "inst")
	e.Interval = 10 * time.Second // long, so we trigger via cancel
	e.OnEvent(validate.Event{Cell: "x", Op: "iter_start", Iteration: 1})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		e.Run(ctx)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run didn't return after cancel")
	}
	if len(srv.calls()) == 0 {
		t.Errorf("expected a final flush on cancel; got 0 pushes")
	}
}
