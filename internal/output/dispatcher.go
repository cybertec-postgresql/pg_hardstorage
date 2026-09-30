// dispatcher.go — Sink interface + dispatcher fan-out of every Event to per-sink ordered queues.
package output

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime/debug"
	"sync"
	"time"
)

// Sink is the asynchronous, system-scoped output plugin tier.
//
// Sinks run alongside the active Renderer: every Event the dispatcher
// renders is also queued for each open sink, which receives its events
// one at a time, in order, on its own delivery goroutine. They
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

// SingleDocumentRenderer is the opt-in a Renderer implements when its
// stdout contract is "exactly one document per invocation" (json, yaml).
// For such renderers the dispatcher never writes streaming Events to
// stdout: a second document there breaks every `cmd -o json | jq`
// consumer, and every command can emit start-up warnings (config,
// sinks, Tier-2 plugin discovery) before its Result. Events go to
// stderr instead, one compact JSON object per line, so stderr stays a
// machine-readable stream and nothing is lost.
type SingleDocumentRenderer interface {
	SingleDocument() bool
}

// DefaultSinkQueueSize bounds how many Events may wait for one Sink.
// When a sink falls this far behind, further events for that sink are
// dropped (and counted) instead of growing memory without limit or
// back-pressuring the foreground command.
const DefaultSinkQueueSize = 1024

// DefaultSinkDrainTimeout bounds how long Close waits for the sinks to
// drain their queues. A sink wedged on a dead endpoint must not turn
// process exit into a hang.
const DefaultSinkDrainTimeout = 10 * time.Second

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
// Each Sink gets one delivery goroutine fed by a bounded queue, so a
// sink sees events one at a time and in the order they were emitted,
// and a slow sink costs at most DefaultSinkQueueSize queued events —
// never an unbounded pile of goroutines calling Emit concurrently.
type Dispatcher struct {
	renderer Renderer
	sinks    []*sinkWorker
	out      io.Writer
	err      io.Writer
	// eventOut / eventsAsLines redirect streaming Events away from
	// stdout; see SingleDocumentRenderer and EventsToStderr.
	eventOut      io.Writer
	eventsAsLines bool
	mu            sync.Mutex
	closed        bool // set under mu by Close; gates further fan-out

	// DrainTimeout bounds Close's wait for sink queues to drain. Zero
	// means DefaultSinkDrainTimeout.
	DrainTimeout time.Duration

	// stop is cancelled when Close gives up on draining, so a sink
	// blocked in a context-aware call (HTTP POST, dial) returns.
	stop       context.Context
	cancelStop context.CancelFunc
}

// sinkWorker is one Sink's ordered delivery lane.
type sinkWorker struct {
	sink    Sink
	queue   chan sinkItem
	done    chan struct{}
	dropped int // events refused because queue was full; guarded by Dispatcher.mu
}

type sinkItem struct {
	ctx context.Context
	ev  *Event
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
	stop, cancel := context.WithCancel(context.Background())
	d := &Dispatcher{renderer: renderer, out: out, err: err, eventOut: out,
		stop: stop, cancelStop: cancel}
	if sd, ok := renderer.(SingleDocumentRenderer); ok && sd.SingleDocument() {
		d.eventOut, d.eventsAsLines = err, true
	}
	return d
}

// EventsToStderr routes every streaming Event to stderr as one compact
// JSON line, whatever the renderer. Used when stdout belongs to another
// protocol (the MCP server's JSON-RPC stream) and a stray event there
// would corrupt it.
func (d *Dispatcher) EventsToStderr() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.eventOut, d.eventsAsLines = d.err, true
}

// AddSink registers a Sink. The same Sink instance can be registered
// multiple times; each registration receives every subsequent event
// (so duplicate registration produces duplicate emissions). Nil is a
// no-op, as is adding to a closed Dispatcher.
func (d *Dispatcher) AddSink(s Sink) {
	if s == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}
	w := &sinkWorker{sink: s, queue: make(chan sinkItem, DefaultSinkQueueSize), done: make(chan struct{})}
	d.sinks = append(d.sinks, w)
	go d.deliver(w)
}

// deliver is a sink's single delivery goroutine: it drains the queue in
// order until Close closes it.
func (d *Dispatcher) deliver(w *sinkWorker) {
	defer close(w.done)
	for it := range w.queue {
		d.emitOne(w.sink, it)
	}
}

// emitOne isolates one Emit so a panicking sink loses that event, not
// its whole delivery lane.
func (d *Dispatcher) emitOne(sink Sink, it sinkItem) {
	defer d.recoverSinkPanic(sink)
	_ = sink.Emit(it.ctx, it.ev)
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
// Streaming Events render to stdout regardless of severity, so a
// pipeline like `pg_hardstorage backup -o ndjson | jq` keeps a single
// clean stream — except under a SingleDocumentRenderer (json, yaml) or
// after EventsToStderr, where stdout is reserved for the one Result and
// events go to stderr as JSON lines.
//
// Sink delivery never blocks the caller: the event is queued on each
// sink's bounded lane, or dropped (and counted) when that lane is full.
func (d *Dispatcher) Event(ctx context.Context, e *Event) error {
	if e == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		// Shutting down: the renderer may be closed and the sink
		// queues are closed (a send would panic).
		return nil
	}
	var renderErr error
	if d.eventsAsLines {
		renderErr = writeEventLine(d.eventOut, e)
	} else {
		renderErr = d.renderer.RenderEvent(d.eventOut, e)
	}
	if len(d.sinks) == 0 {
		return renderErr
	}
	// Freeze the event for the async sinks: the foreground caller returns
	// the moment this method does and may mutate or reuse the original
	// (its Body map) while the delivery goroutines still read it. The
	// snapshot is reachable only by those goroutines, which only read it.
	frozen := e.snapshotForSinks()
	// The caller's context may be cancelled the moment it returns (the
	// command finished, Ctrl-C) while its event still waits in a queue;
	// keep its values but take cancellation from the dispatcher.
	it := sinkItem{ctx: detachedContext{Context: d.stop, values: ctx}, ev: frozen}
	for _, w := range d.sinks {
		select {
		case w.queue <- it:
		default:
			w.dropped++
		}
	}
	return renderErr
}

// writeEventLine encodes e as one compact JSON line — the NDJSON wire
// shape — for events diverted off stdout.
func writeEventLine(w io.Writer, e *Event) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(e)
}

// detachedContext carries a caller's values (trace IDs, …) but takes
// Done/Err/Deadline from the dispatcher's stop context.
type detachedContext struct {
	context.Context
	values context.Context
}

func (c detachedContext) Value(key any) any {
	if c.values == nil {
		return nil
	}
	return c.values.Value(key)
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

// Close drains every sink queue (bounded by DrainTimeout), then closes
// each Sink and the Renderer. Errors from each step are joined so a
// caller can inspect partial failures while still proceeding.
//
// A sink whose queue does not drain in time is reported and NOT closed:
// its Emit is still running, and a Sink must never observe Close racing
// an in-flight Emit. Idempotent: a second Close is a no-op.
func (d *Dispatcher) Close() error {
	// Mark closed under the lock FIRST: Event holds the same lock while
	// it enqueues, so once closed is set no Event can send on a queue we
	// are about to close.
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true
	workers := d.sinks
	for _, w := range workers {
		close(w.queue)
	}
	d.mu.Unlock()
	defer d.cancelStop()

	timeout := d.DrainTimeout
	if timeout <= 0 {
		timeout = DefaultSinkDrainTimeout
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	var errs []error
	var undelivered []map[string]any
	expired := false
	for _, w := range workers {
		if !expired {
			select {
			case <-w.done:
			case <-deadline.C:
				// Budget spent: cancel the stop context so
				// context-aware sinks give up, and stop waiting.
				expired = true
				d.cancelStop()
			}
		}
		select {
		case <-w.done:
			if err := w.sink.Close(); err != nil {
				errs = append(errs, err)
			}
		default:
			errs = append(errs, fmt.Errorf("output: sink %q did not drain within %s; abandoned", w.sink.Name(), timeout))
			undelivered = append(undelivered, map[string]any{
				"sink": w.sink.Name(), "reason": "drain_timeout", "timeout": timeout.String()})
		}
		if w.dropped > 0 {
			errs = append(errs, fmt.Errorf("output: sink %q fell behind; dropped %d event(s)", w.sink.Name(), w.dropped))
			undelivered = append(undelivered, map[string]any{
				"sink": w.sink.Name(), "reason": "queue_full", "dropped": w.dropped})
		}
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	// Tell the operator on the event stream (sinks are gone by now, so
	// this reaches the terminal / stderr only): a sink that silently
	// lost alerts is exactly what an operator must hear about. It rides
	// the event path so it has the same format as every other event.
	for _, body := range undelivered {
		ev := NewEvent(SeverityWarning, "output", "sink.undelivered").WithBody(body)
		if d.eventsAsLines {
			_ = writeEventLine(d.eventOut, ev)
		} else {
			_ = d.renderer.RenderEvent(d.eventOut, ev)
		}
	}
	if d.renderer != nil {
		if err := d.renderer.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
