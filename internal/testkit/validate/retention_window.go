package validate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/report"
)

// runRetentionWindows opens a retention window every opts.RetentionInterval
// until ctx ends. See LoopOptions.RetentionInterval for why retention is
// fleet-wide.
func runRetentionWindows(ctx context.Context, cells []CellRuntime, gate *retentionGate, opts LoopOptions, emit func(Event)) {
	t := time.NewTicker(opts.RetentionInterval)
	defer t.Stop()
	st := newRetentionState(cells, opts.RetentionMaxDeferrals)
	for window := 1; ; window++ {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		runRetentionWindow(ctx, cells, gate, opts.RetentionQuiesceTimeout, window, st, emit)
	}
}

// retentionRepo is one repository the fleet writes to, and the cells
// whose deployments live in it (in fleet order).
type retentionRepo struct {
	key   string
	cells []string
	// deferred counts consecutive windows in which this repository was
	// not garbage-collected for a transient reason.
	deferred int
}

// retentionState carries per-repository deferral accounting across
// windows.
//
// A deferral is individually benign — a lease a fault left behind
// expires within its TTL, a busy fleet drains next time — which is why
// one is an event and not a failure. But "the next window retries" is
// only true if a later window gets through: a lease leaked for good (or
// a fleet that never drains) kept gc away for an entire 8 h run while
// the repository grew, and every window said only retention_deferred.
// maxDeferrals consecutive deferrals of one repository therefore fail
// the run.
type retentionState struct {
	repos        []*retentionRepo // fleet order of first appearance
	byKey        map[string]*retentionRepo
	maxDeferrals int // <= 0: never escalate
}

func newRetentionState(cells []CellRuntime, maxDeferrals int) *retentionState {
	st := &retentionState{byKey: map[string]*retentionRepo{}, maxDeferrals: maxDeferrals}
	for _, c := range cells {
		ra, ok := c.(RetentionApplier)
		if !ok {
			continue
		}
		key := ra.RepoKey()
		r := st.byKey[key]
		if r == nil {
			r = &retentionRepo{key: key}
			st.byKey[key] = r
			st.repos = append(st.repos, r)
		}
		r.cells = append(r.cells, c.Name())
	}
	return st
}

// deferred records a window in which repo was not collected, and fails
// the run once that has happened maxDeferrals windows in a row.
func (st *retentionState) deferred(r *retentionRepo, window int, reason string, emit func(Event)) {
	r.deferred++
	if st.maxDeferrals <= 0 || r.deferred < st.maxDeferrals {
		return
	}
	// A repository shared by several cells is not any one cell's
	// failure; a cell's own repository is that cell's.
	cell := "fleet"
	if len(r.cells) == 1 {
		cell = r.cells[0]
	}
	msg := fmt.Sprintf("gc of the repository of %s was deferred %d windows in a row (last: %s); "+
		"a lease that never expires or a fleet that never drains keeps the repository growing unbounded",
		strings.Join(r.cells, ", "), r.deferred, reason)
	emit(Event{Cell: cell, Op: "retention_deferred_too_long", Iteration: window, Err: msg})
	reportFailure(report.Failure{
		At: time.Now().UTC(), Cell: cell, Iteration: window,
		Kind: "retention", Message: msg,
	})
	r.deferred = 0 // report again only after another full run of deferrals
}

// runRetentionWindow rotates every cell's deployment, then gc's each
// repository the fleet writes to once, with the fleet's backups and
// verifies held.
func runRetentionWindow(ctx context.Context, cells []CellRuntime, gate *retentionGate, quiesce time.Duration, window int, st *retentionState, emit func(Event)) {
	const fleet = "fleet"
	if !gate.quiesce(ctx, quiesce) {
		if ctx.Err() == nil {
			reason := fmt.Sprintf("in-flight backups/verifies did not drain within %s", quiesce)
			emit(Event{Cell: fleet, Op: "retention_deferred", Iteration: window, Detail: reason})
			for _, r := range st.repos {
				st.deferred(r, window, reason, emit)
			}
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

	// gc each repository once, from the first of its cells that is up.
	// Once per repository, not once per window: cells with their own
	// sink have their own repository, and gc'ing only the first cell's
	// left every other one growing for the whole run.
	for _, r := range st.repos {
		ran := false
		for _, c := range gcCandidates {
			ra := c.(RetentionApplier)
			if ra.RepoKey() != r.key {
				continue
			}
			err := ra.GC(ctx)
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, ErrCellNotReady) {
				continue
			}
			ran = true
			switch {
			case errors.Is(err, ErrRetentionDeferred):
				emit(Event{Cell: c.Name(), Op: "retention_deferred", Iteration: window, Err: err.Error()})
				st.deferred(r, window, err.Error(), emit)
			case err != nil:
				r.deferred = 0
				fail(c.Name(), "retention_gc_failed", err)
			default:
				r.deferred = 0
				emit(Event{Cell: c.Name(), Op: "retention_ok", Iteration: window})
			}
			break
		}
		if !ran {
			reason := "no cell was up to run gc"
			emit(Event{Cell: fleet, Op: "retention_deferred", Iteration: window,
				Detail: reason + " (" + strings.Join(r.cells, ", ") + ")"})
			st.deferred(r, window, reason, emit)
		}
	}
}
