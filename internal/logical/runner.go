// runner.go — Runner: supervises N concurrent logical-decoding pipelines with backoff retry.
package logical

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/logical/sinks/chunked"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/logicalreceiver"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo/casdefault"
)

// Runner supervises N concurrent logical-decoding pipelines. The
// CLI's `logical stream <name>` runs ONE stream interactively; the
// agent uses Runner to run every registered stream as a long-lived
// supervised goroutine with backoff-on-error retry.
//
// One goroutine per Stream. A failure in one stream (transient PG
// disconnect, slot drop, sink commit error) doesn't kill any of
// the others — each is independently supervised. ctx cancellation
// cascades cleanly to all goroutines; Run blocks until every
// goroutine exits.
//
// What's deliberately NOT here for:
//
//   - Hot reload: streams added/removed via the registry while
//     Runner is running are NOT auto-picked-up. The agent restart
//     cycle picks up new streams; the operator workflow accepts
//     this.+ will add a registry watcher.
//   - Per-stream concurrency throttling: each stream gets its own
//     PG connection. Operators with hundreds of streams on a
//     single agent should stagger the registry across multiple
//     agents (the same way backup tasks already split).
//   - Health-aware dispatch: a stream that fails N consecutive
//     times still keeps trying with backoff.+ adds a
//     circuit-breaker that quarantines a permanently-failing
//     stream and surfaces it through doctor.
type Runner struct {
	Manager *Manager

	// ConnectionFor resolves a stream to its libpq connection string.
	// Typically backed by the agent's local config map (deployment
	// name → DeploymentConfig.PGConnection).
	ConnectionFor func(s *Stream) string

	// OnEvent receives progress + retry events. Optional; nil
	// discards. Events fire on the per-stream goroutine, so the
	// callback must return promptly.
	OnEvent func(*output.Event)

	// Backoff configures the per-stream retry delay. Zero value
	// uses DefaultBackoff (exponential with full jitter, capped
	// at 5 minutes).
	Backoff Backoff

	// RescanInterval is the cadence at which Run polls the registry
	// for newly-added or removed streams. Default 30s. Set to 0 to
	// disable hot reload (the legacy-prep behaviour: list at
	// startup, never rescan).
	//
	// New streams: a goroutine starts on the next tick.
	// Removed streams: the goroutine's per-stream ctx cancels and
	// the supervisor drains its in-flight batch.
	RescanInterval time.Duration

	// rng is the runtime rand.Rand for jittered backoff. Tests can
	// inject a deterministic source; Run() seeds a default when
	// rng is nil.
	rng *rand.Rand
	mu  sync.Mutex
}

// DefaultRescanInterval is the cadence used when RescanInterval is
// zero. 30 seconds: fast enough that an operator running
// `logical add` while the agent is up sees their stream pick up
// in well under a minute, slow enough that the registry-list
// cost is trivial under any realistic stream count.
const DefaultRescanInterval = 30 * time.Second

// runningStream tracks one in-flight supervisor goroutine. Held
// in the watcher's active map; the cancel func tears down just
// that one stream when the registry stops listing it.
type runningStream struct {
	cancel context.CancelFunc
}

// Backoff is the per-stream retry-delay policy. Exponential with
// full jitter, capped at Max.
type Backoff struct {
	// Initial is the first delay after the first failure. Default
	// 1s.
	Initial time.Duration
	// Max caps the delay. Default 5 minutes.
	Max time.Duration
	// Multiplier compounds Initial → Max. Default 2.
	Multiplier float64
}

// DefaultBackoff is the policy used when Runner.Backoff is the
// zero value. Conservative on the high end so a stream against a
// genuinely-broken upstream doesn't hammer it.
var DefaultBackoff = Backoff{
	Initial:    1 * time.Second,
	Max:        5 * time.Minute,
	Multiplier: 2.0,
}

// nextDelay returns the delay after the n-th consecutive failure
// (n=0 for the first retry). Full-jitter exponential — uniformly
// distributed over [0, exponentialDelay).
func (b Backoff) nextDelay(n int, rng *rand.Rand) time.Duration {
	init := b.Initial
	max := b.Max
	mult := b.Multiplier
	if init <= 0 {
		init = DefaultBackoff.Initial
	}
	if max <= 0 {
		max = DefaultBackoff.Max
	}
	if mult <= 0 {
		mult = DefaultBackoff.Multiplier
	}
	d := float64(init)
	for i := 0; i < n; i++ {
		d *= mult
		if d >= float64(max) {
			d = float64(max)
			break
		}
	}
	// Full-jitter: uniformly over [0, d).
	if rng != nil {
		return time.Duration(rng.Float64() * d)
	}
	return time.Duration(d)
}

// Run starts one goroutine per registered stream, then watches the
// registry for newly-added or removed streams (when RescanInterval
// is non-zero). New streams start a goroutine on the next tick;
// removed streams have their per-stream ctx cancelled and the
// supervisor drains.
//
// Returns nil on clean shutdown (ctx cancelled). All per-stream
// goroutines are joined before Run returns.
func (r *Runner) Run(ctx context.Context) error {
	if r.Manager == nil {
		return errors.New("logical: Runner.Manager is required")
	}
	if r.ConnectionFor == nil {
		return errors.New("logical: Runner.ConnectionFor is required")
	}
	r.mu.Lock()
	if r.rng == nil {
		r.rng = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	r.mu.Unlock()

	rescan := r.RescanInterval
	if rescan == 0 {
		rescan = DefaultRescanInterval
	}

	// Track the running set so the watcher can diff against it.
	// active maps stream name → cancel func; wg counts every
	// goroutine so Run waits for them all on exit.
	active := map[string]runningStream{}
	var wg sync.WaitGroup

	// startStream is the per-stream launch helper used by both the
	// initial run and the watcher's "new stream detected" branch.
	startStream := func(s Stream) {
		streamCtx, cancel := context.WithCancel(ctx)
		active[s.Name] = runningStream{cancel: cancel}
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.superviseStream(streamCtx, &s)
		}()
	}

	// Initial start.
	streams, err := r.Manager.List()
	if err != nil {
		return fmt.Errorf("logical runner: list streams: %w", err)
	}
	for _, s := range streams {
		startStream(s)
	}
	r.emit(output.NewEvent(output.SeverityInfo, "logical.runner", "started").
		WithBody(map[string]any{
			"stream_count":    len(streams),
			"rescan_interval": rescan.String(),
		}))

	// Watcher loop. Disabled when RescanInterval < 0; default cadence
	// otherwise. Stops when ctx is done so we always reach the wg.Wait
	// drain below.
	//
	// The watcher MUST be tracked in wg because it's a writer of
	// `active` (via startStream) — without that, wg could drop to
	// zero between supervisors finishing and a new ticker fire,
	// `wg.Wait()` would return, and the watcher's next startStream
	// call would do `wg.Add(1)` AFTER Wait returned. That's the
	// documented "Add called concurrently with Wait" misuse and
	// triggers a data-race panic. Tracking the watcher means
	// wg.Wait() blocks until BOTH the per-stream supervisors AND
	// the watcher have exited; no late Add can race the Wait.
	if rescan > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.watchRegistry(ctx, rescan, active, startStream)
		}()
	}

	wg.Wait()
	r.emit(output.NewEvent(output.SeverityInfo, "logical.runner", "stopped"))
	return nil
}

// watchRegistry rescans the manager every interval, diffs against
// the active set, starts goroutines for newly-added streams, and
// cancels goroutines for removed streams. Mutates active in place.
//
// Concurrency model: this is the SOLE writer of `active` after Run
// kicks off the initial supervisors. Per-stream supervisors only
// read their own ctx via closure; they never touch the map. So a
// plain map (no extra mutex) is correct as long as watchRegistry
// runs on exactly one goroutine.
func (r *Runner) watchRegistry(ctx context.Context, interval time.Duration, active map[string]runningStream, startStream func(Stream)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		streams, err := r.Manager.List()
		if err != nil {
			r.emit(output.NewEvent(output.SeverityWarning, "logical.runner", "rescan.list_failed").
				WithBody(map[string]any{"error": err.Error()}))
			continue
		}
		// Wanted set.
		wanted := map[string]Stream{}
		for _, s := range streams {
			wanted[s.Name] = s
		}
		// Start newly-added.
		for name, s := range wanted {
			if _, ok := active[name]; ok {
				continue
			}
			r.emit(output.NewEvent(output.SeverityInfo, "logical.runner", "stream.added").
				WithBody(map[string]any{"stream": name, "deployment": s.Deployment}))
			startStream(s)
		}
		// Cancel removed.
		for name, runState := range active {
			if _, ok := wanted[name]; ok {
				continue
			}
			r.emit(output.NewEvent(output.SeverityInfo, "logical.runner", "stream.removed").
				WithBody(map[string]any{"stream": name}))
			runState.cancel()
			delete(active, name)
		}
	}
}

// superviseStream is one stream's lifetime: run-fail-backoff-retry
// until ctx cancels. We treat context.Canceled from the inner Run
// as a clean shutdown (don't retry); every other error triggers
// backoff + retry.
func (r *Runner) superviseStream(ctx context.Context, s *Stream) {
	conn := r.ConnectionFor(s)
	if conn == "" {
		r.emit(output.NewEvent(output.SeverityWarning, "logical.runner", "stream.no_connection").
			WithBody(map[string]any{
				"stream":     s.Name,
				"deployment": s.Deployment,
				"hint":       "the deployment has no pg_connection in the local config; cannot start this stream",
			}))
		return
	}

	failures := 0
	for {
		if ctx.Err() != nil {
			return
		}
		r.emit(output.NewEvent(output.SeverityInfo, "logical.runner", "stream.starting").
			WithBody(map[string]any{
				"stream":     s.Name,
				"deployment": s.Deployment,
				"slot":       s.Slot,
			}))
		err := r.runStreamOnce(ctx, s, conn)
		switch {
		case err == nil:
			// Clean exit (the receiver returned nil). Treated as
			// terminal — receivers shouldn't return nil unless
			// asked to stop (which means ctx is done). Belt-and-
			// braces: re-check ctx before exiting the loop.
			if ctx.Err() != nil {
				return
			}
			r.emit(output.NewEvent(output.SeverityWarning, "logical.runner", "stream.ended_unexpectedly").
				WithBody(map[string]any{"stream": s.Name}))
			return
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			r.emit(output.NewEvent(output.SeverityInfo, "logical.runner", "stream.stopped").
				WithBody(map[string]any{"stream": s.Name}))
			return
		}

		// Transient error → backoff + retry.
		failures++
		r.mu.Lock()
		delay := r.Backoff.nextDelay(failures-1, r.rng)
		r.mu.Unlock()
		r.emit(output.NewEvent(output.SeverityWarning, "logical.runner", "stream.retry").
			WithBody(map[string]any{
				"stream":     s.Name,
				"failures":   failures,
				"error":      err.Error(),
				"backoff_ms": delay.Milliseconds(),
			}))

		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

// runStreamOnce executes one full run of a stream — open repo,
// ensure slot, build sink, connect, run receiver, final flush.
// Returns nil on clean stop (ctx done from receiver) or an error
// the supervisor will retry through.
func (r *Runner) runStreamOnce(ctx context.Context, s *Stream, conn string) error {
	_, sp, err := repo.Open(ctx, s.RepoURL)
	if err != nil {
		return fmt.Errorf("open repo: %w", err)
	}
	defer sp.Close()
	cas := casdefault.New(sp)

	// Ensure the slot exists. Idempotent on already-present.
	{
		c, err := pg.Connect(ctx, conn, pg.ModeReplication)
		if err != nil {
			return fmt.Errorf("connect replication: %w", err)
		}
		if err := logicalreceiver.CreateLogicalSlot(ctx, c, s.Slot, s.Plugin); err != nil {
			c.Close(ctx)
			return fmt.Errorf("create slot %s: %w", s.Slot, err)
		}
		c.Close(ctx)
	}

	sink, err := chunked.New(cas, sp, chunked.Options{
		Deployment: s.Deployment,
		StreamName: s.Name,
		Slot:       s.Slot,
		Plugin:     s.Plugin,
	})
	if err != nil {
		return fmt.Errorf("build sink: %w", err)
	}

	streamConn, err := pg.Connect(ctx, conn, pg.ModeReplication)
	if err != nil {
		return fmt.Errorf("connect replication for stream: %w", err)
	}

	// pgoutput proto_version=2 + named publication. Operators on
	// older PG can override via the+ stream config; for
	// we use the default that works on PG 14+.
	args := []string{
		"proto_version '2'",
		fmt.Sprintf("publication_names '%s'", s.Publication),
	}
	streamErr := logicalreceiver.Stream(ctx, streamConn, logicalreceiver.StreamOptions{
		Slot:                 s.Slot,
		StartLSN:             pglogrepl.LSN(0), // 0 → resume from slot's confirmed_flush
		PluginArgs:           args,
		StatusUpdateInterval: 10 * time.Second,
	}, sink)

	// Final flush so any partial batch durably commits before we
	// return — even on the error path. A flush failure is non-
	// fatal here; the supervisor retries the whole stream and the
	// next run will redo the work from the slot's confirmed_flush.
	//
	// Why a fresh background context instead of the parent ctx:
	// when we land here on a Canceled / DeadlineExceeded ctx, the
	// parent is already done — passing it through would abort the
	// flush mid-write, leaving the sink with partial in-memory
	// state. The detached context keeps the durable commit going
	// regardless of why the parent stream stopped.
	//
	// Why a 30s watchdog: a wedged backend (S3 5xx storm, hung
	// TCP, FUSE bug) would otherwise pin this goroutine forever.
	// Bounding the flush lets a stuck sink fail the supervisor's
	// retry loop cleanly instead of silently leaking the worker.
	flushCtx, cancelFlush := context.WithTimeout(context.Background(), 30*time.Second)
	_ = sink.Flush(flushCtx)
	cancelFlush()

	if streamErr == nil {
		return nil
	}
	if errors.Is(streamErr, context.Canceled) || errors.Is(streamErr, context.DeadlineExceeded) {
		return streamErr
	}
	return fmt.Errorf("stream %s: %w", s.Name, streamErr)
}

// emit forwards an event through OnEvent if set. Best-effort.
func (r *Runner) emit(ev *output.Event) {
	if r.OnEvent == nil {
		return
	}
	r.OnEvent(ev)
}
