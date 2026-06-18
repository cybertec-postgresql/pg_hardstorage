// Package bisect implements the testkit's scenario-aware
// `git bisect` driver.
//
// `pg_hardstorage_testkit scenario bisect --bad HEAD --good
// v0.1.0 --scenario X.scenario.yaml` walks the commit range,
// runs the scenario at each candidate, and reports the first
// commit whose run fails.  Same shape as `git bisect run`,
// but the per-commit step is "rebuild the binary, run the
// scenario harness against it" rather than a generic shell
// script.
//
// We deliberately don't shell to `git bisect run` — that
// command interleaves the bisect-controller's stdout with
// the runner's, and we want clean structured output for
// dashboards.  Instead we drive the bisect by hand:
//
//  1. List the commit range via `git log --pretty=%H bad..good^`.
//  2. Binary-search the list, running the harness at each
//     midpoint.
//  3. Return the first commit whose harness exit is non-zero.
//
// The harness is pluggable so tests don't have to spawn a
// real subprocess: callers pass a `Runner` func that knows
// how to evaluate a candidate commit.
package bisect

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Outcome is the per-commit run verdict.
type Outcome int

const (
	// Good: the scenario passed at this commit (regression
	// not yet introduced).
	Good Outcome = iota
	// Bad: the scenario failed (regression introduced at or
	// before this commit).
	Bad
	// Skip: this commit can't be evaluated (e.g. build broke
	// for unrelated reasons).
	Skip
)

// String returns the lowercase outcome name ("good", "bad",
// "skip") used in audit logs and dashboards.
func (o Outcome) String() string {
	switch o {
	case Good:
		return "good"
	case Bad:
		return "bad"
	case Skip:
		return "skip"
	}
	return fmt.Sprintf("unknown-%d", int(o))
}

// Runner evaluates one commit and returns the outcome.
// Production runners check out the SHA, rebuild the binary,
// then run the scenario harness against it.  Tests inject a
// pure-Go runner that consults a fixture map.
type Runner func(ctx context.Context, sha string) (Outcome, error)

// Result is what Run returns.
type Result struct {
	// FirstBadSHA is the regressing commit, or empty when
	// every candidate was Good (no regression in range) or
	// every candidate was Bad (regression older than the
	// `good` boundary).
	FirstBadSHA string

	// Steps records the per-iteration verdicts so the audit
	// log + dashboards can replay the bisection.
	Steps []Step

	// Skipped records every commit that returned Skip.  An
	// operator with skipped commits in the middle of the
	// range may need to widen the bounds.
	Skipped []string
}

// Step is one bisection iteration.
type Step struct {
	SHA     string  `json:"sha"`
	Outcome Outcome `json:"outcome"`
	Note    string  `json:"note,omitempty"`
}

// Options tunes Run.
type Options struct {
	// CommitRange is the SHAs from `bad` (latest) to one
	// AFTER `good` (oldest).  Both inclusive.  Caller's job
	// to materialise this — we don't shell to git here.
	CommitRange []string

	// Runner evaluates each candidate.
	Runner Runner

	// MaxParallelSkipExpansion bounds the work we'll do
	// re-scanning skipped commits.  Default 8.
	MaxParallelSkipExpansion int
}

// Run is the binary-search driver.  Returns the first
// commit that flips Good → Bad.
//
// Algorithm:
//   - The list is ordered newest..oldest (bad..good).
//   - We binary-search for the boundary between Bad
//     (newer) and Good (older).
//   - A Skip in the middle pushes us out one step at a
//     time until we find a Good or Bad.
func Run(ctx context.Context, opts Options) (Result, error) {
	if opts.Runner == nil {
		return Result{}, errors.New("bisect: nil Runner")
	}
	if len(opts.CommitRange) < 2 {
		return Result{}, errors.New("bisect: need at least 2 commits in range")
	}
	if opts.MaxParallelSkipExpansion <= 0 {
		opts.MaxParallelSkipExpansion = 8
	}
	r := Result{}

	// We assert the boundaries first so an operator with a
	// mis-typed --good gets a fast failure.
	bad, err := opts.Runner(ctx, opts.CommitRange[0])
	if err != nil {
		return r, fmt.Errorf("bisect: evaluate bad-end %s: %w", opts.CommitRange[0], err)
	}
	r.Steps = append(r.Steps, Step{SHA: opts.CommitRange[0], Outcome: bad, Note: "boundary-bad"})
	if bad != Bad {
		return r, fmt.Errorf("bisect: bad-end %s is %s (not Bad) — widen --bad", opts.CommitRange[0], bad)
	}

	good, err := opts.Runner(ctx, opts.CommitRange[len(opts.CommitRange)-1])
	if err != nil {
		return r, fmt.Errorf("bisect: evaluate good-end: %w", err)
	}
	r.Steps = append(r.Steps, Step{SHA: opts.CommitRange[len(opts.CommitRange)-1], Outcome: good, Note: "boundary-good"})
	if good != Good {
		return r, fmt.Errorf("bisect: good-end %s is %s (not Good) — widen --good", opts.CommitRange[len(opts.CommitRange)-1], good)
	}

	lo, hi := 0, len(opts.CommitRange)-1
	for hi-lo > 1 {
		mid := lo + (hi-lo)/2
		sha := opts.CommitRange[mid]
		oc, err := opts.Runner(ctx, sha)
		if err != nil {
			return r, fmt.Errorf("bisect: evaluate %s: %w", sha, err)
		}
		r.Steps = append(r.Steps, Step{SHA: sha, Outcome: oc})
		switch oc {
		case Bad:
			lo = mid
		case Good:
			hi = mid
		case Skip:
			r.Skipped = append(r.Skipped, sha)
			// Drop this index out of the candidate window.
			// We don't reorder — just nudge mid one step
			// closer to lo until we get a Good or Bad.
			next, err := expandSkipped(ctx, opts, mid, lo, hi, &r)
			if err != nil {
				return r, err
			}
			if next.outcome == Bad {
				lo = next.idx
			} else if next.outcome == Good {
				hi = next.idx
			} else {
				return r, fmt.Errorf("bisect: every commit between %d and %d skipped — widen the range or fix the build", lo, hi)
			}
		}
	}

	// `lo` now points at the latest known-Bad; `hi` at the
	// oldest known-Good.  The regressor is at index `lo`.
	r.FirstBadSHA = opts.CommitRange[lo]
	sort.Strings(r.Skipped)
	return r, nil
}

type expandResult struct {
	idx     int
	outcome Outcome
}

// expandSkipped re-evaluates commits adjacent to a Skip
// until it finds a Good or Bad, bounded by the operator's
// MaxParallelSkipExpansion.
func expandSkipped(ctx context.Context, opts Options, mid, lo, hi int, r *Result) (expandResult, error) {
	maxN := opts.MaxParallelSkipExpansion
	for delta := 1; delta <= maxN; delta++ {
		// Check older neighbour first (closer to known-Good).
		for _, idx := range []int{mid + delta, mid - delta} {
			if idx <= lo || idx >= hi {
				continue
			}
			oc, err := opts.Runner(ctx, opts.CommitRange[idx])
			if err != nil {
				return expandResult{}, err
			}
			r.Steps = append(r.Steps, Step{SHA: opts.CommitRange[idx], Outcome: oc, Note: fmt.Sprintf("skip-expand@%d", delta)})
			if oc == Skip {
				continue
			}
			return expandResult{idx: idx, outcome: oc}, nil
		}
	}
	return expandResult{outcome: Skip}, nil
}

// FromMap returns a Runner backed by an in-memory map of
// SHA → Outcome.  Used by tests.
func FromMap(m map[string]Outcome) Runner {
	return func(ctx context.Context, sha string) (Outcome, error) {
		if oc, ok := m[sha]; ok {
			return oc, nil
		}
		return Skip, nil
	}
}

// SafeMap is a thread-safe Runner backing.  Useful when a
// future parallel-bisect mode evaluates multiple candidates
// concurrently.
type SafeMap struct {
	mu sync.Mutex
	m  map[string]Outcome
}

// NewSafeMap returns an empty SafeMap.
func NewSafeMap() *SafeMap { return &SafeMap{m: map[string]Outcome{}} }

// Set records an outcome for a SHA.
func (s *SafeMap) Set(sha string, oc Outcome) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[sha] = oc
}

// Runner returns the bisect Runner.
func (s *SafeMap) Runner() Runner {
	return func(_ context.Context, sha string) (Outcome, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if oc, ok := s.m[sha]; ok {
			return oc, nil
		}
		return Skip, nil
	}
}
