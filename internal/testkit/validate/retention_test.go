package validate_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/report"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/validate"
)

// retentionCell is a FakeCellRuntime that also applies retention.
type retentionCell struct {
	*validate.FakeCellRuntime
	calls atomic.Int32
	err   error
}

func (r *retentionCell) ApplyRetention(context.Context) error {
	r.calls.Add(1)
	return r.err
}

func runRetention(t *testing.T, cell validate.CellRuntime, every int) (*report.Report, []validate.Event) {
	t.Helper()
	validate.ResetForTesting()
	emit, events, mu := collectEvents(t)
	rep, err := validate.Run(context.Background(), validate.RunOptions{
		Seed:     3,
		Duration: 150 * time.Millisecond,
		Loop:     validate.LoopOptions{BackupEvery: 1, VerifyEvery: 2, RetentionEvery: every},
		Faults:   defaultFaults(),
		Cells:    []validate.CellRuntime{cell},
		OnEvent:  emit,
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

func TestRun_RetentionRunsOnCadence(t *testing.T) {
	cell := &retentionCell{FakeCellRuntime: &validate.FakeCellRuntime{NameStr: "c"}}
	rep, evs := runRetention(t, cell, 3)
	if !rep.OverallPass {
		t.Fatalf("retention succeeding must not fail the run: %+v", rep.Failures)
	}
	if cell.calls.Load() == 0 || int(cell.calls.Load()) != countOp(evs, "retention_ok") {
		t.Fatalf("calls=%d retention_ok=%d; want equal and > 0", cell.calls.Load(), countOp(evs, "retention_ok"))
	}
	if iters := countOp(evs, "iter_start"); int(cell.calls.Load()) > iters/3 {
		t.Errorf("retention ran %d times in %d iterations; cadence is every 3", cell.calls.Load(), iters)
	}
}

func TestRun_RetentionDisabledWhenNegative(t *testing.T) {
	cell := &retentionCell{FakeCellRuntime: &validate.FakeCellRuntime{NameStr: "c"}}
	runRetention(t, cell, -1)
	if n := cell.calls.Load(); n != 0 {
		t.Fatalf("RetentionEvery<0 must disable retention; ran %d times", n)
	}
}

// A cell a fault took down is skipped, not failed — same as backup.
func TestRun_RetentionCellDownIsSkipped(t *testing.T) {
	cell := &retentionCell{FakeCellRuntime: &validate.FakeCellRuntime{NameStr: "c"},
		err: fmt.Errorf("%w: gone", validate.ErrCellNotReady)}
	rep, evs := runRetention(t, cell, 2)
	if !rep.OverallPass || countOp(evs, "retention_skipped_cell_down") == 0 {
		t.Fatalf("cell-down retention must be skipped, not failed: pass=%v failures=%+v", rep.OverallPass, rep.Failures)
	}
}

// A retention that fails is a product failure: rotate and gc are the
// product, and a gc that errors under load is exactly what to catch.
func TestRun_RetentionFailureMarksReport(t *testing.T) {
	cell := &retentionCell{FakeCellRuntime: &validate.FakeCellRuntime{NameStr: "c"},
		err: errors.New("gc: boom")}
	rep, evs := runRetention(t, cell, 2)
	if rep.OverallPass || countOp(evs, "retention_failed") == 0 {
		t.Fatalf("failed retention must fail the run: pass=%v", rep.OverallPass)
	}
	if rep.Failures[0].Kind != "retention" {
		t.Errorf("failure kind = %q, want retention", rep.Failures[0].Kind)
	}
}
