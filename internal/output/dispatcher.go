// dispatcher.go — Sink interface + dispatcher fan-out of every Event to per-sink goroutines.
package output

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime/debug"
	"sync"
)

// Sink is the asynchronous, system-scoped output plugin tier.
//
// Sinks run alongside the active Renderer: every Event the dispatcher
// renders is also fanned out to each open sink in its own goroutine. They
// are how Slack / PagerDuty / Jira / syslog / OpenTelemetry-events / etc.
// receive operational events.
//
// In v0.1 the Sink interface is defined here for the dispatcher to plug
// into; concrete implementations land in later slices.
type Sink interface {
	Name() string
	Open(ctx context.Context, cfg map[string]any) error
	Emit(ctx context.Context, ev *Event) error
	Close() error
}

// Dispatcher is the single fan-out point that every CLI command writes
// through. It owns:
//   - one active Renderer (chosen at startup by --output / env / TTY)
//   - any number of Sinks (configured in pg_hardstorage.yaml)
//   - the stdout/stderr writers (so tests can substitute byte buffers)
//
// Calls to Result / Event are serialized through a mutex so the active
// Renderer doesn't need to be goroutine-safe itself; the Renderer
// contract states implementations may assume single-threaded access.
//
// In-flight Sink emissions are tracked by a sync.WaitGroup so Close
// blocks until every Emit returns. Without that, a slow Sink could
// observe a Close call mid-Emit, with undefined per-Sink behaviour.
type Dispatcher struct {
	renderer Renderer
	sinks    []Sink
	out      io.Writer
	err      io.Writer
	mu       sync.Mutex
	closed   bool           // set under mu by Close; gates further fan-out
	emitters sync.WaitGroup // counts in-flight async sink Emit calls
}

// NewDispatcher constructs a Dispatcher with the given renderer.
// out is where success Results and Events land; err is where error
// Results land. Pass os.Stdout / os.Stderr from main; pass byte buffers
// from tests.
func NewDispatcher(renderer Renderer, out, err io.Writer) *Dispatcher {
	if renderer == nil {
		panic("output: NewDispatcher requires a non-nil Renderer")
	}
	if out == nil {
		out = io.Discard
	}
	if err == nil {
		err = out
	}
	return &Dispatcher{renderer: renderer, out: out, err: err}
}

// AddSink registers a Sink. The same Sink instance can be registered
// multiple times; each registration receives every subsequent event
// (so duplicate registration produces duplicate emissions). Nil is a no-op.
func (d *Dispatcher) AddSink(s Sink) {
	if s == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sinks = append(d.sinks, s)
}

// Renderer returns the active renderer (mainly for tests / introspection).
func (d *Dispatcher) Renderer() Renderer {
	return d.renderer
}

// Result renders a one-shot Result and returns any I/O error from rendering.
//
// Routing: a Result whose Error is set goes to stderr; success Results
// go to stdout. This matches Unix convention so `cmd 2>/dev/null`
// behaves as users expect.
func (d *Dispatcher) Result(r *Result) error {
	if r == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	w := d.out
	if r.IsError() {
		w = d.err
	}
	return d.renderer.RenderResult(w, r)
}

// Event renders a streaming Event and (asynchronously) fans it out to all
// registered Sinks. Sink emission errors are not returned — a flaky Sink
// must not break the foreground command. Errors are surfaced via sink-side
// instrumentation instead.
//
// Streaming Events always render to stdout regardless of severity, so a
// pipeline like `pg_hardstorage backup -o ndjson | jq` keeps a single
// clean stream. Diagnostic stderr is reserved for command-level failures
// (handled via Result).
//
// Each Sink Emit runs in its own goroutine; Dispatcher.Close blocks until
// every in-flight Emit returns, so Sinks never observe a Close racing
// with a concurrent Emit on the same instance.
func (d *Dispatcher) Event(ctx context.Context, e *Event) error {
	if e == nil {
		return nil
	}
	d.mu.Lock()
	if d.closed {
		// Dispatcher is shutting down. Don't render to a closing renderer
		// and — critically — don't Add to emitters: Close set d.closed
		// under this same lock before its Wait, so refusing the Add here
		// guarantees every positive Add happened-before that Wait (no
		// "WaitGroup Add concurrent with Wait" race — data-race audit #1).
		d.mu.Unlock()
		return nil
	}
	renderErr := d.renderer.RenderEvent(d.out, e)
	sinks := d.sinks // snapshot under lock
	// Freeze the event for the async sinks: the foreground caller returns
	// the moment this method does and may mutate or reuse the original
	// (its Body map) while these goroutines still read it. The snapshot is
	// reachable only by the sink goroutines, which only read it.
	frozen := e.snapshotForSinks()
	// Track each goroutine before unlocking, so a Close that arrives
	// immediately after will Wait for them.
	d.emitters.Add(len(sinks))
	d.mu.Unlock()

	for _, s := range sinks {
		go func(sink Sink) {
			defer d.emitters.Done()
			defer d.recoverSinkPanic(sink)
			_ = sink.Emit(ctx, frozen)
		}(s)
	}
	return renderErr
}

// recoverSinkPanic recovers from a panic in sink.Emit and writes a
// terse diagnostic to the dispatcher's err stream. We deliberately do
// NOT re-emit the panic as an Event: the fan-out path is exactly what
// just panicked, and a recursive emission risks a panic loop. stderr
// is the right venue for "the system itself is misbehaving" notices.
//
// Without this recover, a panicking sink would crash the entire agent
// — sinks come from third-party plugins (Tier-2 go-plugin) and may
// have bugs we don't control, so the dispatcher must isolate them.
func (d *Dispatcher) recoverSinkPanic(sink Sink) {
	r := recover()
	if r == nil {
		return
	}
	name := "<unknown>"
	if sink != nil {
		name = sink.Name()
	}
	fmt.Fprintf(d.err, "pg_hardstorage: sink %q panicked: %v\n%s\n",
		name, r, debug.Stack())
}

// Close releases the renderer and every sink. Blocks until every in-flight
// asynchronous Emit returns, then closes each Sink and the Renderer.
// Errors from each step are joined so a caller can inspect partial
// failures while still proceeding.
func (d *Dispatcher) Close() error {
	// Mark closed under the lock FIRST, so any Event that has not yet
	// taken the lock sees closed and skips its emitters.Add. Every Add
	// that did run held the lock before us, so it happens-before this
	// point — and therefore before the Wait below. Idempotent: a second
	// Close is a no-op.
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true
	d.mu.Unlock()

	// Wait for in-flight emissions WITHOUT holding the mutex (Emit takes
	// the same mutex briefly). Calling Wait under the lock would deadlock
	// any Emit currently waiting to enter Event.
	d.emitters.Wait()

	d.mu.Lock()
	defer d.mu.Unlock()
	var errs []error
	if d.renderer != nil {
		if err := d.renderer.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	for _, s := range d.sinks {
		if err := s.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
