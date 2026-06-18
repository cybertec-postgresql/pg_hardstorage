package output_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// recordingRenderer captures Renderer calls for assertions.
type recordingRenderer struct {
	mu      sync.Mutex
	results []*output.Result
	events  []*output.Event
	tty     bool
	err     error
}

func (r *recordingRenderer) Name() string      { return "recording" }
func (r *recordingRenderer) SupportsTTY() bool { return r.tty }
func (r *recordingRenderer) Close() error      { return nil }
func (r *recordingRenderer) RenderResult(w io.Writer, x *output.Result) error {
	r.mu.Lock()
	r.results = append(r.results, x)
	r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	_, e := w.Write([]byte(x.Command + "\n"))
	return e
}
func (r *recordingRenderer) RenderEvent(w io.Writer, x *output.Event) error {
	r.mu.Lock()
	r.events = append(r.events, x)
	r.mu.Unlock()
	_, e := w.Write([]byte(x.Op + "\n"))
	return e
}

// recordingSink captures Sink emissions.
type recordingSink struct {
	count atomic.Int32
	done  chan struct{}
}

func newRecordingSink(expect int) *recordingSink {
	return &recordingSink{done: make(chan struct{}, expect)}
}
func (s *recordingSink) Name() string                                   { return "rec" }
func (s *recordingSink) Open(_ context.Context, _ map[string]any) error { return nil }
func (s *recordingSink) Close() error                                   { return nil }
func (s *recordingSink) Emit(_ context.Context, _ *output.Event) error {
	s.count.Add(1)
	s.done <- struct{}{}
	return nil
}

func TestDispatcher_RoutesResultsByErrorState(t *testing.T) {
	rr := &recordingRenderer{}
	var stdout, stderr bytes.Buffer
	d := output.NewDispatcher(rr, &stdout, &stderr)

	if err := d.Result(output.NewResult("status").WithBody("ok")); err != nil {
		t.Fatalf("Result success: %v", err)
	}
	if stdout.String() == "" || stderr.String() != "" {
		t.Errorf("success should land on stdout only; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if err := d.Result(output.NewResult("backup").WithError(output.NewError("x.y", "boom"))); err != nil {
		t.Fatalf("Result error render: %v", err)
	}
	if stderr.String() == "" || stdout.String() != "" {
		t.Errorf("error should land on stderr only; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestDispatcher_EventStaysOnStdout(t *testing.T) {
	rr := &recordingRenderer{}
	var stdout, stderr bytes.Buffer
	d := output.NewDispatcher(rr, &stdout, &stderr)

	for _, sev := range []output.Severity{output.SeverityInfo, output.SeverityWarning, output.SeverityError, output.SeverityCritical} {
		if err := d.Event(context.Background(), output.NewEvent(sev, "c", "op")); err != nil {
			t.Fatalf("event %s: %v", sev, err)
		}
	}
	if stderr.String() != "" {
		t.Errorf("events should not land on stderr; got %q", stderr.String())
	}
	if !strings.Contains(stdout.String(), "op\n") {
		t.Errorf("events missing on stdout; got %q", stdout.String())
	}
}

func TestDispatcher_EventFansOutToSinks(t *testing.T) {
	rr := &recordingRenderer{}
	var stdout, stderr bytes.Buffer
	d := output.NewDispatcher(rr, &stdout, &stderr)

	const n = 50
	s1 := newRecordingSink(n)
	s2 := newRecordingSink(n)
	d.AddSink(s1)
	d.AddSink(s2)

	for i := 0; i < n; i++ {
		_ = d.Event(context.Background(), output.NewEvent(output.SeverityInfo, "c", "op"))
	}

	wait := time.NewTimer(2 * time.Second)
	defer wait.Stop()
	for _, s := range []*recordingSink{s1, s2} {
		got := int32(0)
		for got < n {
			select {
			case <-s.done:
				got++
			case <-wait.C:
				t.Fatalf("sink saw %d/%d events", got, n)
			}
		}
	}
}

func TestDispatcher_NilSinkIgnored(t *testing.T) {
	rr := &recordingRenderer{}
	d := output.NewDispatcher(rr, &bytes.Buffer{}, &bytes.Buffer{})
	d.AddSink(nil)
	if err := d.Event(context.Background(), output.NewEvent(output.SeverityInfo, "c", "op")); err != nil {
		t.Fatal(err)
	}
}

func TestDispatcher_PropagatesRenderError(t *testing.T) {
	rr := &recordingRenderer{err: errors.New("disk full")}
	d := output.NewDispatcher(rr, io.Discard, io.Discard)
	if err := d.Result(output.NewResult("x")); err == nil {
		t.Error("expected render error to propagate")
	}
}

func TestDispatcher_NilRendererPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on nil renderer")
		}
	}()
	output.NewDispatcher(nil, &bytes.Buffer{}, &bytes.Buffer{})
}

func TestDispatcher_NilResultIsNoop(t *testing.T) {
	rr := &recordingRenderer{}
	d := output.NewDispatcher(rr, &bytes.Buffer{}, &bytes.Buffer{})
	if err := d.Result(nil); err != nil {
		t.Errorf("nil result should be a no-op: %v", err)
	}
	if err := d.Event(context.Background(), nil); err != nil {
		t.Errorf("nil event should be a no-op: %v", err)
	}
}

// slowSink mimics a backend that takes a measurable amount of time per
// Emit. We use it to verify that Close blocks until in-flight Emits
// finish — without that, sinks could observe Close racing with Emit on
// the same instance.
type slowSink struct {
	delay      time.Duration
	emitCount  atomic.Int32
	closeCount atomic.Int32
	doneEmit   chan struct{}
	closedAt   atomic.Pointer[time.Time]
	emitEndAt  atomic.Pointer[time.Time]
}

func newSlowSink(delay time.Duration, capacity int) *slowSink {
	return &slowSink{delay: delay, doneEmit: make(chan struct{}, capacity)}
}
func (s *slowSink) Name() string                                   { return "slow" }
func (s *slowSink) Open(_ context.Context, _ map[string]any) error { return nil }
func (s *slowSink) Emit(_ context.Context, _ *output.Event) error {
	s.emitCount.Add(1)
	time.Sleep(s.delay)
	now := time.Now()
	s.emitEndAt.Store(&now)
	select {
	case s.doneEmit <- struct{}{}:
	default:
	}
	return nil
}
func (s *slowSink) Close() error {
	s.closeCount.Add(1)
	now := time.Now()
	s.closedAt.Store(&now)
	return nil
}

// panicSink panics on the first Emit. Used to verify the dispatcher
// recovers from sink panics rather than letting them crash the agent.
type panicSink struct {
	panicValue any
}

func (s *panicSink) Name() string                                   { return "panic" }
func (s *panicSink) Open(_ context.Context, _ map[string]any) error { return nil }
func (s *panicSink) Close() error                                   { return nil }
func (s *panicSink) Emit(_ context.Context, _ *output.Event) error {
	panic(s.panicValue)
}

func TestDispatcher_RecoversFromSinkPanic(t *testing.T) {
	rr := &recordingRenderer{}
	var stdout, stderr bytes.Buffer
	d := output.NewDispatcher(rr, &stdout, &stderr)

	// A panicking sink alongside a healthy one. The healthy sink must
	// still receive the event; the panic must not prevent Close from
	// returning.
	d.AddSink(&panicSink{panicValue: "sink boom"})
	healthy := newRecordingSink(1)
	d.AddSink(healthy)

	if err := d.Event(context.Background(), output.NewEvent(output.SeverityInfo, "c", "op")); err != nil {
		t.Fatalf("event: %v", err)
	}

	// The healthy sink must observe the event (proves the panic in the
	// other sink didn't poison the fan-out).
	select {
	case <-healthy.done:
	case <-time.After(2 * time.Second):
		t.Fatal("healthy sink never received event after sibling sink panicked")
	}

	// Close must return — without recovery the WaitGroup would still
	// be balanced (defer fires on panic), but a re-panic in the
	// goroutine would crash the test process. Recovery proves the
	// program survives.
	if err := d.Close(); err != nil {
		t.Errorf("close: %v", err)
	}

	if !strings.Contains(stderr.String(), "sink \"panic\" panicked") {
		t.Errorf("expected stderr to mention the sink panic; got %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "sink boom") {
		t.Errorf("expected stderr to include the recovered value; got %q", stderr.String())
	}
}

func TestDispatcher_Close_WaitsForInFlightEmits(t *testing.T) {
	rr := &recordingRenderer{}
	d := output.NewDispatcher(rr, &bytes.Buffer{}, &bytes.Buffer{})
	s := newSlowSink(80*time.Millisecond, 4)
	d.AddSink(s)

	// Fire several events without waiting for the goroutines.
	const N = 4
	for i := 0; i < N; i++ {
		_ = d.Event(context.Background(), output.NewEvent(output.SeverityInfo, "c", "op"))
	}

	// Close must block until all Emits finish — even though we called it
	// immediately after Event, before the slow Emits could possibly have
	// returned.
	if err := d.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if got := s.emitCount.Load(); got != N {
		t.Errorf("Emit count = %d, want %d", got, N)
	}
	if got := s.closeCount.Load(); got != 1 {
		t.Errorf("Close count = %d, want 1", got)
	}
	closedAt := s.closedAt.Load()
	emitEnd := s.emitEndAt.Load()
	if closedAt == nil || emitEnd == nil {
		t.Fatal("missing timestamps")
	}
	if closedAt.Before(*emitEnd) {
		t.Errorf("Close ran before Emit completed: closed=%v, last_emit=%v", closedAt, emitEnd)
	}
}
