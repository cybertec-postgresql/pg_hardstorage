package validate_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/inject"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/report"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/validate"
)

// retentionCell is a FakeCellRuntime that also applies retention, and
// whose backups take a moment so a window has something to drain.
type retentionCell struct {
	*validate.FakeCellRuntime
	inflight  *atomic.Int32 // backups in flight across the fleet
	violation *atomic.Int32 // gc calls that saw a backup in flight
	rotates   atomic.Int32
	gcs       atomic.Int32
	gcErr     error
	// gcFn, when set, decides the n-th gc's result instead of gcErr.
	gcFn func(n int32) error
	// repo names the repository this cell's deployment lives in; ""
	// is the one repository the rest of the fleet shares.
	repo string
}

func (r *retentionCell) RepoKey() string { return r.repo }

func (r *retentionCell) TakeBackup(ctx context.Context) (string, error) {
	r.inflight.Add(1)
	defer r.inflight.Add(-1)
	time.Sleep(2 * time.Millisecond)
	return r.FakeCellRuntime.TakeBackup(ctx)
}

// ApplyFault takes a moment and counts as in flight, like a backup:
// a window must never overlap one (a fault killed a window's rotate).
func (r *retentionCell) ApplyFault(ctx context.Context, action string) (inject.Recovery, error) {
	r.inflight.Add(1)
	defer r.inflight.Add(-1)
	time.Sleep(2 * time.Millisecond)
	return r.FakeCellRuntime.ApplyFault(ctx, action)
}

func (r *retentionCell) Rotate(context.Context) error {
	r.rotates.Add(1)
	if r.inflight.Load() != 0 {
		r.violation.Add(1)
	}
	return nil
}

func (r *retentionCell) GC(context.Context) error {
	n := r.gcs.Add(1)
	if r.inflight.Load() != 0 {
		r.violation.Add(1)
	}
	if r.gcFn != nil {
		return r.gcFn(n)
	}
	return r.gcErr
}

func newRetentionFleet(n int, gcErr error) ([]*retentionCell, *atomic.Int32) {
	inflight, violation := &atomic.Int32{}, &atomic.Int32{}
	var cells []*retentionCell
	for i := range n {
		cells = append(cells, &retentionCell{
			FakeCellRuntime: &validate.FakeCellRuntime{NameStr: fmt.Sprintf("c%d", i)},
			inflight:        inflight, violation: violation, gcErr: gcErr,
		})
	}
	return cells, violation
}

func runRetention(t *testing.T, cells []*retentionCell, interval time.Duration) (*report.Report, []validate.Event) {
	t.Helper()
	validate.ResetForTesting()
	emit, events, mu := collectEvents(t)
	var rts []validate.CellRuntime
	for _, c := range cells {
		rts = append(rts, c)
	}
	rep, err := validate.Run(context.Background(), validate.RunOptions{
		Seed:     3,
		Duration: 300 * time.Millisecond,
		Loop: validate.LoopOptions{BackupEvery: 1, VerifyEvery: 2, FaultProbability: 0.5,
			RetentionInterval: interval, RetentionQuiesceTimeout: time.Second},
		Faults:  defaultFaults(),
		Cells:   rts,
		OnEvent: emit,
	})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	return rep, append([]validate.Event(nil), (*events)...)
}

func countOp(evs []validate.Event, op string) int {
	n := 0
	for _, e := range evs {
		if e.Op == op {
			n++
		}
	}
	return n
}

// Every window rotates every cell and gc's the shared repository once,
// with no backup in flight while gc runs.
func TestRun_RetentionWindowRotatesEveryCellAndGCsOnceWhileQuiet(t *testing.T) {
	cells, violation := newRetentionFleet(3, nil)
	rep, evs := runRetention(t, cells, 40*time.Millisecond)
	if !rep.OverallPass {
		t.Fatalf("successful retention must not fail the run: %+v", rep.Failures)
	}
	windows := countOp(evs, "retention_ok")
	if windows == 0 {
		t.Fatal("no retention window completed")
	}
	var gcs int32
	for _, c := range cells {
		gcs += c.gcs.Load()
		if int(c.rotates.Load()) < windows {
			t.Errorf("%s rotated %d times in %d windows", c.Name(), c.rotates.Load(), windows)
		}
	}
	if int(gcs) != windows {
		t.Errorf("gc ran %d times in %d windows; want once per window", gcs, windows)
	}
	if v := violation.Load(); v != 0 {
		t.Fatalf("rotate/gc ran %d time(s) with a backup or fault in flight", v)
	}
	if countOp(evs, "backup_completed") == 0 || countOp(evs, "fault_apply") == 0 {
		t.Fatal("no backups or no faults ran — the windows starved the fleet, or the test proves nothing about faults")
	}
}

func TestRun_RetentionDisabledWhenNegative(t *testing.T) {
	cells, _ := newRetentionFleet(1, nil)
	runRetention(t, cells, -1)
	if n := cells[0].rotates.Load() + cells[0].gcs.Load(); n != 0 {
		t.Fatalf("RetentionInterval<0 must disable retention; %d calls", n)
	}
}

// gc refusing over a live lease is the product working: deferred, not
// failed — as long as a later window gets through.
func TestRun_RetentionDeferredIsNotAFailure(t *testing.T) {
	cells, _ := newRetentionFleet(2, nil)
	for _, c := range cells {
		c.gcFn = func(n int32) error {
			if n%2 == 1 {
				return fmt.Errorf("%w: lease", validate.ErrRetentionDeferred)
			}
			return nil
		}
	}
	rep, evs := runRetention(t, cells, 40*time.Millisecond)
	if !rep.OverallPass || countOp(evs, "retention_deferred") == 0 {
		t.Fatalf("a deferred gc must be recorded and must not fail: pass=%v failures=%+v", rep.OverallPass, rep.Failures)
	}
}

// A deferral that never clears is not "the next window retries": a
// backup lease leaked by a killed backup would keep gc away for the
// whole 8h run while the repository grew, and every window said only
// retention_deferred. After RetentionMaxDeferrals (default 4)
// consecutive deferrals of one repository, the run fails.
func TestRun_RetentionDeferredForeverFailsTheRun(t *testing.T) {
	cells, _ := newRetentionFleet(2, fmt.Errorf("%w: lease", validate.ErrRetentionDeferred))
	rep, evs := runRetention(t, cells, 30*time.Millisecond)
	if countOp(evs, "retention_deferred") < 4 {
		t.Fatalf("fixture ran only %d deferred windows; need at least 4", countOp(evs, "retention_deferred"))
	}
	if rep.OverallPass {
		t.Fatal("gc was deferred in every window; the run must fail rather than hide a leaked lease")
	}
	var kinds []string
	for _, f := range rep.Failures {
		kinds = append(kinds, f.Kind)
	}
	if len(kinds) == 0 || kinds[0] != "retention" {
		t.Errorf("failure kinds = %v, want retention", kinds)
	}
}

// Cells with their own sinks have their own repositories. gc ran once per
// window from the first cell that was up, so every other repository was
// never collected.
func TestRun_RetentionGCsEveryDistinctRepository(t *testing.T) {
	cells, _ := newRetentionFleet(3, nil)
	cells[2].repo = "s3://own-sink"
	_, evs := runRetention(t, cells, 40*time.Millisecond)
	windows := countOp(evs, "retention_window_open")
	if windows == 0 {
		t.Fatal("no retention window opened")
	}
	// The run can end inside the last window, before its gc's.
	oncePerWindow := func(n int32) bool { return int(n) <= windows && int(n) >= windows-1 && n > 0 }
	if shared := cells[0].gcs.Load() + cells[1].gcs.Load(); !oncePerWindow(shared) {
		t.Errorf("shared repository gc'd %d times in %d windows; want once per window", shared, windows)
	}
	if own := cells[2].gcs.Load(); !oncePerWindow(own) {
		t.Errorf("c2's own repository gc'd %d times in %d windows; want once per window", own, windows)
	}
}

// Any other gc error is a product failure.
func TestRun_RetentionGCFailureMarksReport(t *testing.T) {
	cells, _ := newRetentionFleet(2, errors.New("gc: boom"))
	rep, evs := runRetention(t, cells, 40*time.Millisecond)
	if rep.OverallPass || countOp(evs, "retention_gc_failed") == 0 {
		t.Fatalf("a failed gc must fail the run: pass=%v", rep.OverallPass)
	}
	if rep.Failures[0].Kind != "retention" {
		t.Errorf("failure kind = %q, want retention", rep.Failures[0].Kind)
	}
}

// Cells must not all restore-verify at the same iteration: each verify
// materialises a whole cluster, and in lockstep they stacked 4x12 GB.
func TestRun_CellsVerifyAtDifferentPhases(t *testing.T) {
	validate.ResetForTesting()
	emit, events, mu := collectEvents(t)
	var cells []validate.CellRuntime
	for _, n := range []string{"ubuntu-2204-pg15-arm", "rockylinux-9-pg16-arm", "debian-12-pg17-arm", "ubuntu-2204-pg18-arm"} {
		cells = append(cells, &validate.FakeCellRuntime{NameStr: n})
	}
	if _, err := validate.Run(context.Background(), validate.RunOptions{
		Seed: 1, Duration: 200 * time.Millisecond,
		Loop:   validate.LoopOptions{BackupEvery: 1, VerifyEvery: 25, RetentionInterval: -1},
		Faults: defaultFaults(), Cells: cells, OnEvent: emit,
	}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	first := map[string]int{}
	for _, e := range *events {
		if e.Op == "verify_started" {
			if _, ok := first[e.Cell]; !ok {
				first[e.Cell] = e.Iteration % 25
			}
		}
	}
	phases := map[int]bool{}
	for _, p := range first {
		phases[p] = true
	}
	if len(first) < 2 || len(phases) < 2 {
		t.Fatalf("verify phases per cell = %v; cells must not verify in lockstep", first)
	}
}
