// wal_stream_progress.go — periodic `wal.stream.progress` event ticker for `wal stream`.
package cli

// Periodic progress reporter for `pg_hardstorage wal stream`.
// Implements the operator-facing piece of issue #53: when the
// streamer is running, emit one `wal.stream.progress` event per
// --status-interval so the interactive output shows the segment
// just committed, the partial PG is currently writing, and the
// recent throughput — instead of looking idle between the one-
// shot `wal.stream.starting` event and the eventual `stopped`.

import (
	"context"
	"time"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
)

// syncedLSNSource is the narrow read-only surface the progress
// ticker needs from a walsink.Sink.  Declared locally so tests
// can drive the ticker without instantiating a real Sink (which
// would need a CAS, a StoragePlugin, and a live PG connection).
type syncedLSNSource interface {
	SyncedLSN() pglogrepl.LSN
	SegmentSize() int64
}

// walStreamProgressTicker drives one `wal.stream.progress` event
// per interval for the lifetime of a single stream attempt.
//
// Goroutine model: a single background goroutine reads
// sink.SyncedLSN() each tick and computes throughput / segment
// names from the deltas.  The goroutine exits when either
// ctx is cancelled OR Stop is called — Stop blocks until any
// in-flight tick has emitted, so the caller can rely on
// "no progress event arrives after Stop returns".
//
// No locks: SyncedLSN reads an atomic.Uint64 inside the Sink,
// and the goroutine's per-tick state (prevLSN, prevAt) lives
// only on its own stack.
type walStreamProgressTicker struct {
	interval   time.Duration
	sink       syncedLSNSource
	emit       func(*output.Event)
	deployment string
	timeline   uint32
	startLSN   pglogrepl.LSN

	// now is the clock source; production wires time.Now,
	// tests inject a fake.  Same posture as the `clock` field
	// in other test-friendly code in this codebase.
	now func() time.Time

	stop chan struct{}
	done chan struct{}
}

// newWalStreamProgressTicker constructs a ticker that, once
// Start() is called, will emit one progress event per
// `interval` until Stop() or ctx-cancel.
//
// emit is called from the ticker goroutine.  Production passes
// the same `emit` closure used by the rest of `wal stream` so
// the events flow through the dispatcher's renderer and JSON-
// vs-text routing without special-casing.
//
// startLSN is the LSN the current stream attempt started at;
// the first tick's `bytes_advanced_total` is measured from
// this baseline, every subsequent tick continues to use it.
// `bytes_advanced_interval` always covers the most recent
// interval only.
func newWalStreamProgressTicker(
	interval time.Duration,
	sink syncedLSNSource,
	emit func(*output.Event),
	deployment string,
	timeline uint32,
	startLSN pglogrepl.LSN,
) *walStreamProgressTicker {
	return &walStreamProgressTicker{
		interval:   interval,
		sink:       sink,
		emit:       emit,
		deployment: deployment,
		timeline:   timeline,
		startLSN:   startLSN,
		now:        time.Now,
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
	}
}

// Start launches the ticker goroutine.  Non-blocking.
// Must be paired with Stop on the same instance.
//
// ctx cancellation is honoured at the next tick (no per-event
// preemption: a tick fired moments before ctx.Done would still
// emit).  This is intentional — operators reading the output
// want the last-known-good throughput sample, not a missing
// one because the stream happened to end mid-tick.
func (p *walStreamProgressTicker) Start(ctx context.Context) {
	go p.run(ctx)
}

// Stop signals the ticker to exit and blocks until it has.
// Idempotent: calling Stop twice on the same ticker returns
// immediately the second time (the close on the already-closed
// `stop` channel is guarded by Once-style semantics — see run).
func (p *walStreamProgressTicker) Stop() {
	select {
	case <-p.stop:
		// Already stopping.
	default:
		close(p.stop)
	}
	<-p.done
}

// run is the ticker goroutine body.  Exits on stop or ctx-Done.
//
// A non-positive interval is a no-op (the goroutine exits
// immediately).  This lets callers wire the ticker
// unconditionally and rely on --status-interval=0 to silence
// progress events without a separate disable path.
func (p *walStreamProgressTicker) run(ctx context.Context) {
	defer close(p.done)
	if p.interval <= 0 {
		return
	}
	t := time.NewTicker(p.interval)
	defer t.Stop()

	// prevLSN/prevAt are the baseline for the NEXT tick's
	// interval-local throughput.  Initialise from startLSN /
	// "now" — the first tick's interval is one full ticker
	// period, which is what an operator expects.
	prevLSN := p.startLSN
	prevAt := p.now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.stop:
			return
		case tickAt := <-t.C:
			curLSN := p.sink.SyncedLSN()
			elapsed := tickAt.Sub(prevAt)
			p.emit(buildProgressEvent(
				p.deployment, p.timeline, p.startLSN, prevLSN, curLSN, elapsed, p.sink.SegmentSize()))
			prevLSN = curLSN
			prevAt = tickAt
		}
	}
}

// buildProgressEvent constructs one `wal.stream.progress` event
// from the (startLSN, prevLSN, curLSN, elapsed) tuple.  Pure
// function; pulled out of the ticker for direct unit-testing.
//
// Body fields:
//
//	synced_lsn               — sink.SyncedLSN() at this tick
//	bytes_advanced_total     — curLSN - startLSN (running total
//	                           for this stream attempt)
//	bytes_advanced_interval  — curLSN - prevLSN (this tick only)
//	bytes_per_second         — interval throughput, integer
//	                           bytes/s; omitted when elapsed <= 0
//	tick_interval_ms         — elapsed.Milliseconds(), so
//	                           operators can compute throughput
//	                           themselves if they prefer
//	last_segment_streamed    — file-name of the segment whose
//	                           ending LSN is at curLSN; omitted
//	                           when curLSN == 0 (no segment has
//	                           committed yet on this attempt)
//	current_partial_segment  — file-name of the segment PG is
//	                           currently writing (i.e. the next
//	                           one after last_segment_streamed)
//
// SyncedLSN is segment-aligned whenever it is non-zero: the
// Sink advances it only after a segment commits, and a commit
// ends exactly at a segment boundary.  This is what lets us
// compute the last-streamed segment name from a single LSN
// without tracking the in-flight partial separately.
func buildProgressEvent(
	deployment string,
	timeline uint32,
	startLSN, prevLSN, curLSN pglogrepl.LSN,
	elapsed time.Duration,
	segSize int64,
) *output.Event {
	// During the warm-up window between START_REPLICATION and the
	// first segment commit, SyncedLSN is 0 while startLSN is
	// wherever PG was asked to resume.  An unsigned subtraction
	// would yield a huge garbage value (underflow); a signed one
	// yields a negative number.  Neither is what an operator
	// wants to see — "bytes advanced" reads as a non-negative
	// counter.  Clamp both totals to a floor of 0 so the
	// pre-first-commit ticks display "0" rather than "-16777216".
	totalAdvanced := int64(0)
	if uint64(curLSN) > uint64(startLSN) {
		totalAdvanced = int64(uint64(curLSN) - uint64(startLSN))
	}
	intervalAdvanced := int64(0)
	if uint64(curLSN) > uint64(prevLSN) {
		intervalAdvanced = int64(uint64(curLSN) - uint64(prevLSN))
	}
	body := map[string]any{
		"synced_lsn":              curLSN.String(),
		"bytes_advanced_total":    totalAdvanced,
		"bytes_advanced_interval": intervalAdvanced,
		"tick_interval_ms":        elapsed.Milliseconds(),
	}
	if ms := elapsed.Milliseconds(); ms > 0 {
		// integer division on the rate avoids reporting
		// 17.4 bytes/s of float noise when delta is small.
		body["bytes_per_second"] = intervalAdvanced * 1000 / ms
	}
	// last_segment_streamed: the segment whose ENDING LSN is
	// SyncedLSN.  When SyncedLSN is 0 the stream has not yet
	// committed any segment on this attempt; omit the field
	// rather than name segment "00000004000000000000FFFFFFFFF"
	// (segment index ^uint64(0)) which would be very confusing.
	segSize = walsink.NormSegmentSize(segSize)
	if uint64(curLSN) > 0 {
		lastSeg := (uint64(curLSN) - 1) / uint64(segSize)
		body["last_segment_streamed"] = walsink.SegmentFileName(timeline, lastSeg, segSize)
	}
	// current_partial_segment: the one PG is currently writing
	// into.  When SyncedLSN is segment-aligned (the typical
	// case), partial = SyncedLSN / segmentSize.  This always
	// emits — even on a brand-new attempt with SyncedLSN=0,
	// the partial is "segment 0 of this timeline."
	partialSeg := uint64(curLSN) / uint64(segSize)
	body["current_partial_segment"] = walsink.SegmentFileName(timeline, partialSeg, segSize)

	return output.NewEvent(output.SeverityInfo, "wal.stream", "progress").
		WithSubject(output.Subject{
			Deployment: deployment,
			Timeline:   timeline,
			LSN:        curLSN.String(),
		}).
		WithBody(body)
}
