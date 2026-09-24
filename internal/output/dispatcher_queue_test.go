package output_test

import (
	"bytes"
	"context"
	stdjson "encoding/json"
	"io"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// singleDocRenderer is a recordingRenderer that declares the
// one-document-per-invocation stdout contract (as json / yaml do).
type singleDocRenderer struct{ recordingRenderer }

func (*singleDocRenderer) SingleDocument() bool { return true }

// TestDispatcher_SingleDocumentRenderer_EventsLeaveStdout pins the JSON
// contract: under a single-document renderer, stdout carries only the
// Result; streaming events go to stderr as one JSON object per line.
func TestDispatcher_SingleDocumentRenderer_EventsLeaveStdout(t *testing.T) {
	var stdout, stderr bytes.Buffer
	d := output.NewDispatcher(&singleDocRenderer{}, &stdout, &stderr)
	for _, op := range []string{"one", "two"} {
		if err := d.Event(context.Background(), output.NewEvent(output.SeverityWarning, "config", op)); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.Result(output.NewResult("status").WithBody("ok")); err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); got != "status\n" {
		t.Errorf("stdout must carry only the Result; got %q", got)
	}
	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 NDJSON event lines on stderr; got %q", stderr.String())
	}
	for i, want := range []string{"one", "two"} {
		var ev output.Event
		if err := stdjson.Unmarshal([]byte(lines[i]), &ev); err != nil {
			t.Fatalf("stderr line %d is not a JSON event: %v (%q)", i, err, lines[i])
		}
		if ev.Op != want || ev.Component != "config" {
			t.Errorf("line %d = %s.%s, want config.%s", i, ev.Component, ev.Op, want)
		}
	}
}

// orderSink records the order events arrive in and the peak number of
// concurrent Emit calls.
type orderSink struct {
	mu       sync.Mutex
	seen     []int
	inFlight atomic.Int32
	peak     atomic.Int32
}

func (s *orderSink) Name() string                                   { return "order" }
func (s *orderSink) Open(_ context.Context, _ map[string]any) error { return nil }
func (s *orderSink) Close() error                                   { return nil }
func (s *orderSink) Emit(_ context.Context, ev *output.Event) error {
	n := s.inFlight.Add(1)
	for {
		p := s.peak.Load()
		if n <= p || s.peak.CompareAndSwap(p, n) {
			break
		}
	}
	time.Sleep(200 * time.Microsecond)
	s.mu.Lock()
	s.seen = append(s.seen, ev.Body.(map[string]any)["seq"].(int))
	s.mu.Unlock()
	s.inFlight.Add(-1)
	return nil
}

// TestDispatcher_SinkDeliveryIsSerialAndOrdered: a sink receives its
// events one at a time and in emission order. Sinks are not required to
// be goroutine-safe, and an alert stream delivered out of order ("resolved"
// before "firing") is wrong.
func TestDispatcher_SinkDeliveryIsSerialAndOrdered(t *testing.T) {
	d := output.NewDispatcher(&recordingRenderer{}, io.Discard, io.Discard)
	s := &orderSink{}
	d.AddSink(s)
	const n = 200
	for i := 0; i < n; i++ {
		_ = d.Event(context.Background(), output.NewEvent(output.SeverityInfo, "c", "op").
			WithBody(map[string]any{"seq": i}))
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if p := s.peak.Load(); p != 1 {
		t.Errorf("peak concurrent Emit = %d, want 1", p)
	}
	if len(s.seen) != n {
		t.Fatalf("sink saw %d events, want %d", len(s.seen), n)
	}
	for i, v := range s.seen {
		if v != i {
			t.Fatalf("event %d delivered at position %d (out of order): %v", v, i, s.seen[:i+1])
		}
	}
}

// stuckSink blocks every Emit until release is closed, ignoring ctx.
type stuckSink struct {
	release chan struct{}
	closed  atomic.Bool
}

func (s *stuckSink) Name() string                                   { return "stuck" }
func (s *stuckSink) Open(_ context.Context, _ map[string]any) error { return nil }
func (s *stuckSink) Close() error                                   { s.closed.Store(true); return nil }
func (s *stuckSink) Emit(_ context.Context, _ *output.Event) error {
	<-s.release
	return nil
}

// TestDispatcher_StuckSinkIsBounded: a sink that never returns must not
// make the dispatcher pile up one goroutine per event.
func TestDispatcher_StuckSinkIsBounded(t *testing.T) {
	d := output.NewDispatcher(&recordingRenderer{}, io.Discard, io.Discard)
	s := &stuckSink{release: make(chan struct{})}
	defer close(s.release)
	d.AddSink(s)
	before := runtime.NumGoroutine()
	for i := 0; i < 5000; i++ {
		_ = d.Event(context.Background(), output.NewEvent(output.SeverityInfo, "c", "op"))
	}
	if grew := runtime.NumGoroutine() - before; grew > 10 {
		t.Errorf("goroutines grew by %d for 5000 events to one stuck sink; fan-out is unbounded", grew)
	}
}

// TestDispatcher_CloseGivesUpOnStuckSink: Close waits at most
// DrainTimeout, reports the stuck sink, and does not call its Close while
// its Emit is still running.
func TestDispatcher_CloseGivesUpOnStuckSink(t *testing.T) {
	d := output.NewDispatcher(&recordingRenderer{}, io.Discard, io.Discard)
	d.DrainTimeout = 100 * time.Millisecond
	s := &stuckSink{release: make(chan struct{})}
	defer close(s.release)
	d.AddSink(s)
	_ = d.Event(context.Background(), output.NewEvent(output.SeverityInfo, "c", "op"))

	done := make(chan error, 1)
	go func() { done <- d.Close() }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), `sink "stuck" did not drain`) {
			t.Errorf("Close error = %v, want a did-not-drain report", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung on a stuck sink")
	}
	if s.closed.Load() {
		t.Error("stuck sink was Closed while its Emit was still in flight")
	}
}

// ctxSink blocks until its context is cancelled.
type ctxSink struct{ gotCancel atomic.Bool }

func (s *ctxSink) Name() string                                   { return "ctx" }
func (s *ctxSink) Open(_ context.Context, _ map[string]any) error { return nil }
func (s *ctxSink) Close() error                                   { return nil }
func (s *ctxSink) Emit(ctx context.Context, _ *output.Event) error {
	<-ctx.Done()
	s.gotCancel.Store(true)
	return ctx.Err()
}

// TestDispatcher_QueuedEventSurvivesCallerCancel: the caller's context
// ending (the command returned) must not cancel delivery of an event
// still queued; only Close giving up does.
func TestDispatcher_QueuedEventSurvivesCallerCancel(t *testing.T) {
	d := output.NewDispatcher(&recordingRenderer{}, io.Discard, io.Discard)
	d.DrainTimeout = 200 * time.Millisecond
	s := &ctxSink{}
	d.AddSink(s)
	ctx, cancel := context.WithCancel(context.Background())
	_ = d.Event(ctx, output.NewEvent(output.SeverityInfo, "c", "op"))
	cancel()
	time.Sleep(50 * time.Millisecond)
	if s.gotCancel.Load() {
		t.Fatal("caller cancellation reached a queued sink delivery")
	}
	_ = d.Close()
	// The cancel is delivered as Close gives up; the Emit then returns
	// on its own goroutine.
	for deadline := time.Now().Add(2 * time.Second); !s.gotCancel.Load() && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	if !s.gotCancel.Load() {
		t.Error("Close's drain timeout did not cancel the blocked Emit")
	}
}
