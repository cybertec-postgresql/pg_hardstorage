package cli

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/walsink"
)

// fakeSyncedLSNSource lets the ticker tests drive what
// SyncedLSN() returns at each call without touching a real
// walsink.Sink (which would need a CAS, a StoragePlugin and a
// live PG connection).  Atomic loads/stores so the production
// ticker goroutine reads safely while the test thread updates.
type fakeSyncedLSNSource struct {
	lsn     atomic.Uint64
	segSize atomic.Int64
}

func (f *fakeSyncedLSNSource) SyncedLSN() pglogrepl.LSN {
	return pglogrepl.LSN(f.lsn.Load())
}

// SegmentSize satisfies syncedLSNSource; the fake streams at the default
// 16 MiB unless a test overrides segSize.
func (f *fakeSyncedLSNSource) SegmentSize() int64 {
	if s := f.segSize.Load(); s != 0 {
		return s
	}
	return walsink.SegmentSize
}

func (f *fakeSyncedLSNSource) set(v uint64) { f.lsn.Store(v) }

// captureEmit collects every output.Event the code under test
// emits.  The ticker calls emit from its goroutine; the slice
// is guarded by a mutex.
type captureEmit struct {
	mu     sync.Mutex
	events []*output.Event
}

func (c *captureEmit) fn() func(*output.Event) {
	return func(e *output.Event) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.events = append(c.events, e)
	}
}

func (c *captureEmit) snapshot() []*output.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*output.Event, len(c.events))
	copy(out, c.events)
	return out
}

// --- buildProgressEvent ------------------------------------------------

// Body fields documented in wal_stream_progress.go's doc comment
// are LOAD-BEARING — operators and the documentation page rely
// on the names and types.  These tests pin them.

func TestBuildProgressEvent_BodyFields(t *testing.T) {
	// Start LSN = 14/D3000000 → 0x14D3000000 bytes.
	// Prev  LSN = 14/D4000000 → start + 16 MiB (one segment).
	// Cur   LSN = 14/D6000000 → start + 48 MiB (two more segments).
	// Elapsed   = 10 s — matches the default --status-interval.
	start := pglogrepl.LSN(0x14D3000000)
	prev := pglogrepl.LSN(0x14D4000000)
	cur := pglogrepl.LSN(0x14D6000000)
	elapsed := 10 * time.Second

	ev := buildProgressEvent("db1", 4, start, prev, cur, elapsed, walsink.SegmentSize)

	if ev.Op != "progress" || ev.Component != "wal.stream" {
		t.Errorf("event op/component: got %s/%s want progress/wal.stream", ev.Op, ev.Component)
	}
	if ev.Severity != output.SeverityInfo {
		t.Errorf("severity = %s, want info", ev.Severity)
	}
	if ev.Subject.Deployment != "db1" || ev.Subject.Timeline != 4 {
		t.Errorf("subject deployment/timeline = %q/%d", ev.Subject.Deployment, ev.Subject.Timeline)
	}
	b := ev.Body.(map[string]any)

	// String fields.
	if got := b["synced_lsn"].(string); got != cur.String() {
		t.Errorf("synced_lsn = %q want %q", got, cur.String())
	}

	// bytes_advanced_total = cur - start = 48 MiB.
	if got := b["bytes_advanced_total"].(int64); got != int64(uint64(cur)-uint64(start)) {
		t.Errorf("bytes_advanced_total = %d want %d", got, uint64(cur)-uint64(start))
	}
	// bytes_advanced_interval = cur - prev = 32 MiB.
	if got := b["bytes_advanced_interval"].(int64); got != int64(uint64(cur)-uint64(prev)) {
		t.Errorf("bytes_advanced_interval = %d want %d", got, uint64(cur)-uint64(prev))
	}
	// tick_interval_ms = elapsed in ms.
	if got := b["tick_interval_ms"].(int64); got != elapsed.Milliseconds() {
		t.Errorf("tick_interval_ms = %d want %d", got, elapsed.Milliseconds())
	}
	// bytes_per_second over 10 s of 32 MiB delta:
	// 32 * 1024 * 1024 / 10 = 3 355 443 bytes/s (integer floor).
	want := int64(uint64(cur)-uint64(prev)) * 1000 / elapsed.Milliseconds()
	if got := b["bytes_per_second"].(int64); got != want {
		t.Errorf("bytes_per_second = %d want %d", got, want)
	}
	// last_segment_streamed = segment whose ENDING LSN is cur.
	// cur = 0x14D6000000; SegmentSize = 0x1000000.  segNum
	// (zero-indexed) of the segment ENDING at cur is
	// (cur - 1) / SegmentSize = 0x14D5. Timeline = 4.
	wantLastSeg := walsink.SegmentFileName(4, (uint64(cur)-1)/walsink.SegmentSize, walsink.SegmentSize)
	if got := b["last_segment_streamed"].(string); got != wantLastSeg {
		t.Errorf("last_segment_streamed = %q want %q", got, wantLastSeg)
	}
	// current_partial_segment = the one PG is now writing into.
	// At a segment-aligned cur this is cur / SegmentSize = 0x14D6.
	wantPartial := walsink.SegmentFileName(4, uint64(cur)/walsink.SegmentSize, walsink.SegmentSize)
	if got := b["current_partial_segment"].(string); got != wantPartial {
		t.Errorf("current_partial_segment = %q want %q", got, wantPartial)
	}
}

func TestBuildProgressEvent_ZeroLSNOmitsLastSegment(t *testing.T) {
	// At the very start of a fresh stream, SyncedLSN is 0:
	// no segment has been committed yet.  The event still
	// emits, but the `last_segment_streamed` field must be
	// absent (naming "segment FFFFFFFF" would be very
	// confusing in the operator's terminal).  The partial
	// is segment 0 of the timeline.
	ev := buildProgressEvent("db1", 4, 0, 0, 0, time.Second, walsink.SegmentSize)
	b := ev.Body.(map[string]any)
	if _, ok := b["last_segment_streamed"]; ok {
		t.Errorf("last_segment_streamed must be omitted when SyncedLSN=0; got %v", b["last_segment_streamed"])
	}
	if got := b["current_partial_segment"].(string); got != walsink.SegmentFileName(4, 0, walsink.SegmentSize) {
		t.Errorf("current_partial_segment at LSN=0 should name segment 0; got %q", got)
	}
}

func TestBuildProgressEvent_ZeroElapsedOmitsBytesPerSecond(t *testing.T) {
	// Defensive: elapsed=0 (clock resolution / instant tick)
	// would divide by zero in the throughput calc; the helper
	// must omit the field instead of dividing.
	ev := buildProgressEvent("db1", 4, 0, 0, pglogrepl.LSN(0x1000000), 0, walsink.SegmentSize)
	b := ev.Body.(map[string]any)
	if _, ok := b["bytes_per_second"]; ok {
		t.Errorf("bytes_per_second must be omitted when elapsed=0; got %v", b["bytes_per_second"])
	}
	// tick_interval_ms still present (=0) — operators want to
	// see "zero-duration tick" rather than have the field vanish.
	if got, ok := b["tick_interval_ms"].(int64); !ok || got != 0 {
		t.Errorf("tick_interval_ms must be present and 0 on zero-elapsed tick; got %v", b["tick_interval_ms"])
	}
}

func TestBuildProgressEvent_NoAdvanceStillEmits(t *testing.T) {
	// When SyncedLSN has not advanced (a stalled stream), the
	// event MUST still emit — operators are watching for "is the
	// agent alive at all?" precisely in this case.  Body shows
	// zero advance and the same partial segment.
	at := pglogrepl.LSN(0x14D5000000)
	ev := buildProgressEvent("db1", 4, at, at, at, 10*time.Second, walsink.SegmentSize)
	b := ev.Body.(map[string]any)
	if got := b["bytes_advanced_interval"].(int64); got != 0 {
		t.Errorf("bytes_advanced_interval = %d, want 0 on a stalled tick", got)
	}
	if got := b["bytes_per_second"].(int64); got != 0 {
		t.Errorf("bytes_per_second = %d, want 0 on a stalled tick", got)
	}
}

func TestBuildProgressEvent_PreCommitWarmupClampsToZero(t *testing.T) {
	// Pre-first-commit window: PG was asked to resume at
	// startLSN > 0 but SyncedLSN is still 0 (no segment has
	// committed yet on this attempt).  The honest delta is
	// negative (curLSN - startLSN underflows in unsigned).
	// buildProgressEvent must clamp to 0 so the operator sees
	// "0 bytes" rather than "-16777216 bytes" — the latter
	// reads as a bug in the streamer, the former reads as
	// "we're waiting for the first commit."
	start := pglogrepl.LSN(0x1000000) // arbitrary non-zero start
	ev := buildProgressEvent("db1", 4, start, 0, 0, time.Second, walsink.SegmentSize)
	b := ev.Body.(map[string]any)
	if got := b["bytes_advanced_total"].(int64); got != 0 {
		t.Errorf("bytes_advanced_total = %d, want 0 during pre-commit warm-up", got)
	}
	if got := b["bytes_advanced_interval"].(int64); got != 0 {
		t.Errorf("bytes_advanced_interval = %d, want 0 during pre-commit warm-up", got)
	}
	if got := b["bytes_per_second"].(int64); got != 0 {
		t.Errorf("bytes_per_second = %d, want 0 during pre-commit warm-up", got)
	}
}

// --- ticker goroutine -------------------------------------------------

func TestWalStreamProgressTicker_EmitsOncePerInterval(t *testing.T) {
	src := &fakeSyncedLSNSource{}
	cap := &captureEmit{}
	// Tiny interval so the test finishes promptly; the production
	// default is 10 s but the goroutine logic is interval-agnostic.
	interval := 20 * time.Millisecond
	tk := newWalStreamProgressTicker(interval, src, cap.fn(), "db1", 4, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tk.Start(ctx)
	// Drive the sink: bump SyncedLSN by one segment, wait for
	// at least two ticks, then stop.  ~80 ms gives the ticker
	// time to fire ~4 times — well above the floor of 1.
	src.set(walsink.SegmentSize)
	time.Sleep(80 * time.Millisecond)
	tk.Stop()

	got := cap.snapshot()
	if len(got) < 2 {
		t.Fatalf("expected at least 2 progress events in 80 ms at 20 ms cadence; got %d", len(got))
	}
	// Every event must be a wal.stream.progress.
	for i, ev := range got {
		if ev.Component != "wal.stream" || ev.Op != "progress" {
			t.Errorf("event %d: component/op = %s/%s want wal.stream/progress", i, ev.Component, ev.Op)
		}
	}
}

func TestWalStreamProgressTicker_StopBlocksUntilGoroutineExits(t *testing.T) {
	src := &fakeSyncedLSNSource{}
	cap := &captureEmit{}
	tk := newWalStreamProgressTicker(10*time.Millisecond, src, cap.fn(), "db1", 4, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tk.Start(ctx)
	// Give the goroutine a moment to start.
	time.Sleep(15 * time.Millisecond)

	// Stop must return promptly; if it deadlocks we hit the
	// test's package-level timeout.  Measure to assert a
	// reasonable bound (well under the ticker interval, since
	// Stop short-circuits the goroutine's select instead of
	// waiting for the next tick).
	stopStart := time.Now()
	tk.Stop()
	if elapsed := time.Since(stopStart); elapsed > 100*time.Millisecond {
		t.Errorf("Stop took %s, expected <100ms", elapsed)
	}
}

func TestWalStreamProgressTicker_StopIsIdempotent(t *testing.T) {
	src := &fakeSyncedLSNSource{}
	cap := &captureEmit{}
	tk := newWalStreamProgressTicker(10*time.Millisecond, src, cap.fn(), "db1", 4, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tk.Start(ctx)
	tk.Stop()
	// Calling Stop again must not panic on close-of-closed-channel.
	tk.Stop()
}

func TestWalStreamProgressTicker_CtxCancelExitsTheGoroutine(t *testing.T) {
	src := &fakeSyncedLSNSource{}
	cap := &captureEmit{}
	tk := newWalStreamProgressTicker(10*time.Millisecond, src, cap.fn(), "db1", 4, 0)

	ctx, cancel := context.WithCancel(context.Background())
	tk.Start(ctx)
	// Cancel the ctx INSTEAD of calling Stop.  The goroutine
	// must still exit promptly so the defer-close on `done`
	// fires.  Stop() then no-ops, but we still call it to
	// keep the contract symmetric.
	cancel()
	tk.Stop()
}

func TestWalStreamProgressTicker_NonPositiveIntervalIsNoop(t *testing.T) {
	src := &fakeSyncedLSNSource{}
	cap := &captureEmit{}
	// A zero interval is the "silence progress" contract for the
	// CLI wiring (--status-interval=0 silences the existing PG
	// standby-status updates and SHOULD silence our progress too).
	tk := newWalStreamProgressTicker(0, src, cap.fn(), "db1", 4, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tk.Start(ctx)
	time.Sleep(50 * time.Millisecond)
	tk.Stop()
	if got := cap.snapshot(); len(got) != 0 {
		t.Errorf("zero-interval ticker emitted %d events; want 0", len(got))
	}
}

func TestWalStreamProgressTicker_AdvancesFromStartLSN(t *testing.T) {
	// bytes_advanced_total is anchored at startLSN — so a tick
	// after the sink advances must report the FULL delta from
	// startLSN, not from prev.
	src := &fakeSyncedLSNSource{}
	cap := &captureEmit{}
	interval := 20 * time.Millisecond
	const startLSN = uint64(0x100000000)
	tk := newWalStreamProgressTicker(interval, src, cap.fn(), "db1", 4, pglogrepl.LSN(startLSN))

	src.set(startLSN + walsink.SegmentSize*3) // advanced 48 MiB
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tk.Start(ctx)
	time.Sleep(60 * time.Millisecond)
	tk.Stop()

	got := cap.snapshot()
	if len(got) == 0 {
		t.Fatal("expected at least one event")
	}
	first := got[0].Body.(map[string]any)
	if total := first["bytes_advanced_total"].(int64); total != int64(walsink.SegmentSize*3) {
		t.Errorf("first tick bytes_advanced_total = %d, want %d", total, walsink.SegmentSize*3)
	}
}

// --- CLI flag wiring --------------------------------------------------

// TestNewWalStreamCmd_VerboseFlagRegistered locks the contract
// that `pg_hardstorage wal stream` exposes --verbose / -v.
// Operators rely on the short form for terminal use; a future
// refactor must not silently drop it.
func TestNewWalStreamCmd_VerboseFlagRegistered(t *testing.T) {
	c := newWalStreamCmd()
	f := c.Flags().Lookup("verbose")
	if f == nil {
		t.Fatal("--verbose flag not registered on `wal stream`")
	}
	if f.Shorthand != "v" {
		t.Errorf("--verbose shorthand = %q, want %q", f.Shorthand, "v")
	}
	if f.DefValue != "false" {
		t.Errorf("--verbose default = %q, want %q", f.DefValue, "false")
	}
	if !strings.Contains(f.Usage, "wal.stream.progress") {
		t.Errorf("--verbose help should reference wal.stream.progress; got %q", f.Usage)
	}
}
