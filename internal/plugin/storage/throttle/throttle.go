// Package throttle implements a bandwidth-cap middleware that wraps
// a StoragePlugin and rate-limits write bytes.
//
// The plan calls this out under: "Egress shaping per repo per
// time-of-day. Bandwidth caps to avoid blowing through cloud-egress
// budget at month-end." This package ships the cap primitive; the
// time-of-day layer (config-driven scheduling) lands on top of it
// when there's a config consumer.
//
// First consumer: `pg_hardstorage repo replicate --max-mbps N`. An
// operator running cross-region replicate from cron wants to bound
// the WAN egress. Wrapping the destination's StoragePlugin with a
// Throttle caps the throughput at the chosen rate.
//
// Implementation: token-bucket. Tokens (bytes) accrue at
// BytesPerSecond up to BurstBytes. Every Put reads its body in
// chunks via a wrapping io.Reader that acquires tokens before
// returning bytes upstream. Get / Stat / List / Delete /
// RenameIfNotExists / SetRetention pass through unchanged — the
// design intent is to cap egress on writes; reads are free.
//
// Concurrent puts share the bucket: two parallel uploaders at the
// same cap split the bandwidth roughly evenly, with a small
// transient overshoot at the start (each acquirer can pull burst
// tokens once before the other waits).
//
// Context cancellation: a long sleep waiting for tokens is
// preempted by ctx.Done; the throttledReader returns ctx.Err on
// the next Read call.
package throttle

import (
	"context"
	"io"
	"iter"
	"sync"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// DefaultChunkSize is the unit of throttling — every Read against
// the wrapped body reads up to this many bytes, then waits for that
// many tokens. Smaller chunks smooth the rate at the cost of more
// scheduler hops; 64 KiB is a good middle ground (matches most
// storage plugins' buffer sizes).
const DefaultChunkSize = 64 * 1024

// Throttle wraps a StoragePlugin with a bandwidth cap on writes.
//
// Construct with New. The zero value is NOT usable — bps and the
// bucket state must be initialised through New.
type Throttle struct {
	inner storage.StoragePlugin

	// bpsFn returns the current bytes-per-second cap. For static
	// caps it returns a constant; for time-of-day schedules it
	// consults a Schedule via the wrapper's clock. Returning <=0
	// means "unbounded right now" — acquire short-circuits.
	bpsFn func() int64

	// unconditionallyUnbounded is set to true when the wrapper
	// was constructed with bytesPerSecond=0 AND no schedule, which
	// lets Put skip wrapping the body reader entirely.
	unconditionallyUnbounded bool

	burst int64 // max bucket capacity in bytes

	chunkSize int

	mu         sync.Mutex
	tokens     float64
	lastRefill time.Time

	// Time + sleep functions, overridable for tests via WithClock.
	nowFn   func() time.Time
	sleepFn func(d time.Duration)
}

// Option tunes a Throttle at construction.
type Option func(*Throttle)

// WithBurst sets the bucket capacity in bytes. Default is one
// second's worth of bandwidth (== bps for the static-cap path; for
// the schedule path the operator should pass a value sized to the
// peak rate they expect).
func WithBurst(bytes int64) Option {
	return func(t *Throttle) { t.burst = bytes }
}

// WithChunkSize sets the per-Read chunk granularity. Default 64 KiB.
func WithChunkSize(n int) Option {
	return func(t *Throttle) { t.chunkSize = n }
}

// WithClock overrides the time + sleep functions. Tests use this to
// get deterministic throttling without real-time waits.
func WithClock(now func() time.Time, sleep func(time.Duration)) Option {
	return func(t *Throttle) {
		t.nowFn = now
		t.sleepFn = sleep
	}
}

// WithSchedule replaces the static cap with a Schedule-driven cap.
// Each acquire consults s.BPSAt(now()) so transitions between
// windows take effect mid-Put. The Throttle becomes "always
// metered" — Put always wraps the body reader, even when the
// schedule currently returns 0 (unbounded), so an entrance into
// a capped window starts shaping immediately.
//
// Burst is NOT inferred from the schedule — operators with a
// peak-rate schedule should pass WithBurst sized to the peak so
// short Puts don't pay an unfair stall at window transitions.
func WithSchedule(s *Schedule) Option {
	return func(t *Throttle) {
		// Capture t by reference so the closure picks up t.nowFn
		// even when WithSchedule is applied before WithClock.
		t.bpsFn = func() int64 { return s.BPSAt(t.nowFn()) }
		t.unconditionallyUnbounded = false
	}
}

// New wraps inner and rate-limits write bytes to bytesPerSecond.
// Pass <=0 to disable throttling entirely (the wrapper becomes a
// transparent pass-through with negligible overhead).
//
// To use a time-of-day schedule instead of a static cap, pass
// WithSchedule(s). When both are present the schedule wins (its
// BPSAt result drives the rate; bytesPerSecond becomes informational
// for the burst default).
func New(inner storage.StoragePlugin, bytesPerSecond int64, opts ...Option) *Throttle {
	staticBPS := bytesPerSecond
	t := &Throttle{
		inner:                    inner,
		bpsFn:                    func() int64 { return staticBPS },
		unconditionallyUnbounded: staticBPS <= 0,
		burst:                    staticBPS,
		chunkSize:                DefaultChunkSize,
		nowFn:                    time.Now,
		sleepFn:                  time.Sleep,
	}
	for _, o := range opts {
		o(t)
	}
	if t.burst < t.chunkSize64() {
		t.burst = t.chunkSize64()
	}
	// Start with a full bucket so a small first Put doesn't pay an
	// immediate stall. (Subsequent reads pay as they go.)
	t.tokens = float64(t.burst)
	return t
}

func (t *Throttle) chunkSize64() int64 { return int64(t.chunkSize) }

// acquire blocks until n tokens are available + consumes them.
// Concurrent calls share the bucket; a deficit is borrowed against
// future tokens (subsequent acquirers wait longer). Returns
// ctx.Err() if ctx is cancelled while waiting.
//
// When the active rate is 0 (unbounded — either statically or via
// the current schedule window), acquire is a fast-path no-op that
// also resets the bucket math so a future capped window starts
// from a clean state.
func (t *Throttle) acquire(ctx context.Context, n int) error {
	if n <= 0 {
		return nil
	}
	bps := t.bpsFn()
	if bps <= 0 {
		// Unbounded right now. Reset bucket math so the next
		// capped window doesn't credit tokens for the unbounded
		// gap (which would defeat the cap).
		t.mu.Lock()
		t.lastRefill = time.Time{}
		t.tokens = float64(t.burst)
		t.mu.Unlock()
		return nil
	}
	t.mu.Lock()
	now := t.nowFn()
	if !t.lastRefill.IsZero() {
		elapsed := now.Sub(t.lastRefill).Seconds()
		t.tokens += elapsed * float64(bps)
		if t.tokens > float64(t.burst) {
			t.tokens = float64(t.burst)
		}
	}
	t.lastRefill = now
	// Always consume; if we go negative, that's the deficit we
	// need to wait off.
	t.tokens -= float64(n)
	var wait time.Duration
	if t.tokens < 0 {
		secs := -t.tokens / float64(bps)
		wait = time.Duration(secs * float64(time.Second))
	}
	t.mu.Unlock()

	if wait <= 0 {
		return ctx.Err()
	}
	// Honour context cancellation: short-circuit if already done.
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	t.sleepFn(wait)
	return ctx.Err()
}

// throttledReader wraps an io.Reader so each Read call reads at
// most chunkSize bytes and acquires that many tokens from the
// throttle. Tokens are acquired AFTER the source read so the
// throttle ledger reflects bytes that actually flowed; a partial
// read (n < requested) is correctly accounted for.
type throttledReader struct {
	ctx context.Context
	src io.Reader
	t   *Throttle
}

// Read implements io.Reader. Each call reads at most chunkSize bytes
// from the source, then acquires that many tokens from the throttle
// so the ledger reflects bytes that actually flowed (partial reads
// are accounted for).
func (r *throttledReader) Read(p []byte) (int, error) {
	limit := r.t.chunkSize
	if limit == 0 || limit > len(p) {
		limit = len(p)
	}
	n, err := r.src.Read(p[:limit])
	if n > 0 {
		if aerr := r.t.acquire(r.ctx, n); aerr != nil {
			// If we acquired-and-failed (ctx cancelled mid-sleep),
			// surface the cancellation. The bytes we read are
			// dropped (not handed to the caller) so the inner
			// Put's body matches the bytes the caller saw flowing.
			return 0, aerr
		}
	}
	return n, err
}

// --- StoragePlugin pass-through methods -----------------------------

// Name returns the inner plugin's name. We don't synthesise a
// "throttle/<name>" name because the wrapper is transparent — the
// storage backend identity for downstream callers is unchanged.
func (t *Throttle) Name() string { return t.inner.Name() }

// Open delegates. Throttle has no open-time state of its own; the
// bucket is live from construction.
func (t *Throttle) Open(ctx context.Context, cfg storage.StorageConfig) error {
	return t.inner.Open(ctx, cfg)
}

// Put wraps the body in a throttledReader so the inner plugin's Put
// pulls bytes at the configured rate. When the wrapper is
// unconditionally unbounded (constructed with bytesPerSecond=0 and
// no schedule), Put delegates without wrapping.
func (t *Throttle) Put(ctx context.Context, key string, r io.Reader, opts storage.PutOptions) (storage.PutResult, error) {
	if t.unconditionallyUnbounded {
		return t.inner.Put(ctx, key, r, opts)
	}
	tr := &throttledReader{ctx: ctx, src: r, t: t}
	return t.inner.Put(ctx, key, tr, opts)
}

// Get delegates without throttling — egress shaping is about
// uploads.
func (t *Throttle) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	return t.inner.Get(ctx, key)
}

// Stat delegates.
func (t *Throttle) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	return t.inner.Stat(ctx, key)
}

// List delegates.
func (t *Throttle) List(ctx context.Context, prefix string) iter.Seq2[storage.ObjectInfo, error] {
	return t.inner.List(ctx, prefix)
}

// Delete delegates.
func (t *Throttle) Delete(ctx context.Context, key string) error {
	return t.inner.Delete(ctx, key)
}

// RenameIfNotExists delegates. Renames are zero-byte at the network
// layer (object stores implement them as a copy + delete on the
// backend's own bandwidth, not ours), so they're not throttled.
func (t *Throttle) RenameIfNotExists(ctx context.Context, src, dst string) error {
	return t.inner.RenameIfNotExists(ctx, src, dst)
}

// SetRetention delegates.
func (t *Throttle) SetRetention(ctx context.Context, key string, until time.Time, mode storage.WORMMode) error {
	return t.inner.SetRetention(ctx, key, until, mode)
}

// Capabilities delegates. The throttle wrapper doesn't change which
// optional features the backend supports.
func (t *Throttle) Capabilities() storage.Capabilities {
	return t.inner.Capabilities()
}

// Barrier forwards to the wrapped plugin. A barrier moves no body
// bytes, so the bandwidth cap does not apply.
func (t *Throttle) Barrier(ctx context.Context) error {
	return t.inner.Barrier(ctx)
}

// Close delegates.
func (t *Throttle) Close() error { return t.inner.Close() }

// Region returns the inner plugin's region if it implements
// RegionAware; otherwise RegionUnknown. Implementing this lets the
// residency-check helper see through the wrapper.
func (t *Throttle) Region() string {
	return storage.RegionOf(t.inner)
}

// Compile-time assertions that we satisfy StoragePlugin.
var _ storage.StoragePlugin = (*Throttle)(nil)
