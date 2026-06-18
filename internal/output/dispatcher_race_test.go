package output_test

import (
	"context"
	"io"
	"sync"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// bodyReadingSink reads the event's Body map in a tight loop on the
// async fan-out goroutine — exactly what a real sink (serialize to JSON,
// forward to Slack/S3) does. Used to expose a race against a caller that
// mutates the original event's Body after Event() returns.
type bodyReadingSink struct {
	started chan struct{}
	once    sync.Once
}

func (s *bodyReadingSink) Name() string                                   { return "bodyreader" }
func (s *bodyReadingSink) Open(_ context.Context, _ map[string]any) error { return nil }
func (s *bodyReadingSink) Close() error                                   { return nil }
func (s *bodyReadingSink) Emit(_ context.Context, ev *output.Event) error {
	s.once.Do(func() { close(s.started) })
	m, _ := ev.Body.(map[string]any)
	sink := 0
	for i := 0; i < 100_000; i++ {
		for _, v := range m { // read every key — races a concurrent writer
			if n, ok := v.(int); ok {
				sink += n
			}
		}
	}
	_ = sink
	return nil
}

// TestDispatcher_EventBodyIsolatedFromCaller pins data-race audit #1: the
// dispatcher hands sinks a frozen snapshot, so a caller mutating the
// original event's Body map after Event() returns cannot race the
// in-flight Emit. Run under -race; against the unfixed dispatcher (which
// handed sinks the live event) this reports a read/write data race on the
// Body map.
func TestDispatcher_EventBodyIsolatedFromCaller(t *testing.T) {
	d := output.NewDispatcher(&recordingRenderer{}, io.Discard, io.Discard)
	sink := &bodyReadingSink{started: make(chan struct{})}
	d.AddSink(sink)

	body := map[string]any{"a": 0, "b": 0, "c": 0}
	ev := output.NewEvent(output.SeverityInfo, "test", "race").WithBody(body)

	if err := d.Event(context.Background(), ev); err != nil {
		t.Fatalf("Event: %v", err)
	}
	// Once the sink goroutine is reading the (snapshot of the) body, hammer
	// the ORIGINAL map. With the freeze this touches a different map than
	// the sink reads; without it, this is a textbook concurrent map
	// read/write.
	<-sink.started
	for i := 1; i <= 100_000; i++ {
		body["a"] = i
		body["b"] = i
		body["c"] = i
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestDispatcher_ConcurrentEventAndClose pins the WaitGroup half of #1:
// Events racing Close must not trip "WaitGroup Add concurrent with Wait".
// Close marks the dispatcher closed under the lock before Wait, so any
// Event that hasn't taken the lock skips its emitters.Add. Against the
// unfixed code this can panic ("WaitGroup misuse: Add called concurrently
// with Wait") and races the counter under -race.
func TestDispatcher_ConcurrentEventAndClose(t *testing.T) {
	for iter := 0; iter < 200; iter++ {
		d := output.NewDispatcher(&recordingRenderer{}, io.Discard, io.Discard)
		d.AddSink(&bodyReadingSinkFast{})

		ev := output.NewEvent(output.SeverityInfo, "test", "close-race").
			WithBody(map[string]any{"k": 1})

		var wg sync.WaitGroup
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = d.Event(context.Background(), ev)
			}()
		}
		// Close races the in-flight Events.
		_ = d.Close()
		wg.Wait()
	}
}

// TestDispatcher_EventAfterCloseIsNoOp: once closed, Event neither
// renders nor fans out, and returns nil.
func TestDispatcher_EventAfterCloseIsNoOp(t *testing.T) {
	rr := &recordingRenderer{}
	d := output.NewDispatcher(rr, io.Discard, io.Discard)
	sink := newRecordingSink(1)
	d.AddSink(sink)

	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := d.Event(context.Background(), output.NewEvent(output.SeverityInfo, "t", "after-close")); err != nil {
		t.Errorf("Event after Close should be a no-op nil; got %v", err)
	}
	if got := sink.count.Load(); got != 0 {
		t.Errorf("sink emitted %d events after Close; want 0", got)
	}
	rr.mu.Lock()
	nEvents := len(rr.events)
	rr.mu.Unlock()
	if nEvents != 0 {
		t.Errorf("renderer rendered %d events after Close; want 0", nEvents)
	}
	// Close is idempotent.
	if err := d.Close(); err != nil {
		t.Errorf("second Close should be a no-op nil; got %v", err)
	}
}

// bodyReadingSinkFast is a trivial sink for the close-race stress test —
// it must return promptly so Close doesn't block the loop.
type bodyReadingSinkFast struct{}

func (bodyReadingSinkFast) Name() string                                   { return "fast" }
func (bodyReadingSinkFast) Open(_ context.Context, _ map[string]any) error { return nil }
func (bodyReadingSinkFast) Close() error                                   { return nil }
func (bodyReadingSinkFast) Emit(_ context.Context, _ *output.Event) error  { return nil }
