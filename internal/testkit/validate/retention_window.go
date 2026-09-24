package validate

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/report"
)

// runRetentionWindows opens a retention window every opts.RetentionInterval
// until ctx ends. See LoopOptions.RetentionInterval for why retention is
// fleet-wide.
func runRetentionWindows(ctx context.Context, cells []CellRuntime, gate *retentionGate, opts LoopOptions, emit func(Event)) {
	t := time.NewTicker(opts.RetentionInterval)
	defer t.Stop()
	for window := 1; ; window++ {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		runRetentionWindow(ctx, cells, gate, opts.RetentionQuiesceTimeout, window, emit)
	}
}

// runRetentionWindow rotates every cell's deployment, then gc's the
// shared repository once, with the fleet's backups and verifies held.
func runRetentionWindow(ctx context.Context, cells []CellRuntime, gate *retentionGate, quiesce time.Duration, window int, emit func(Event)) {
	const fleet = "fleet"
	if !gate.quiesce(ctx, quiesce) {
		if ctx.Err() == nil {
			emit(Event{Cell: fleet, Op: "retention_deferred", Iteration: window,
				Detail: fmt.Sprintf("in-flight backups/verifies did not drain within %s", quiesce)})
		}
		return
	}
	defer gate.resume()
	emit(Event{Cell: fleet, Op: "retention_window_open", Iteration: window})

	// fail records a real retention failure against the cell that ran it.
	fail := func(cell, op string, err error) {
		emit(Event{Cell: cell, Op: op, Iteration: window, Err: err.Error()})
		reportFailure(report.Failure{
			At: time.Now().UTC(), Cell: cell, Iteration: window,
			Kind: "retention", Message: err.Error(),
		})
	}

	var gcCandidates []CellRuntime
	for _, c := range cells {
		ra, ok := c.(RetentionApplier)
		if !ok {
			continue
		}
		err := ra.Rotate(ctx)
		switch {
		case ctx.Err() != nil:
			return
		case errors.Is(err, ErrCellNotReady):
			emit(Event{Cell: c.Name(), Op: "retention_rotate_skipped_cell_down", Iteration: window})
		case err != nil:
			fail(c.Name(), "retention_rotate_failed", err)
		default:
			gcCandidates = append(gcCandidates, c)
		}
	}

	// gc the shared repository once, from the first cell that is up.
	for _, c := range gcCandidates {
		err := c.(RetentionApplier).GC(ctx)
		switch {
		case ctx.Err() != nil:
			return
		case errors.Is(err, ErrCellNotReady):
			continue
		case errors.Is(err, ErrRetentionDeferred):
			emit(Event{Cell: c.Name(), Op: "retention_deferred", Iteration: window, Err: err.Error()})
		case err != nil:
			fail(c.Name(), "retention_gc_failed", err)
		default:
			emit(Event{Cell: c.Name(), Op: "retention_ok", Iteration: window})
		}
		return
	}
	emit(Event{Cell: fleet, Op: "retention_deferred", Iteration: window,
		Detail: "no cell was up to run gc"})
}
