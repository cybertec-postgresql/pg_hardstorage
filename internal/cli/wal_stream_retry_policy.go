package cli

// wal_stream_retry_policy.go — the retry loop's DECISIONS, extracted
// pure so tests drive the real thing.
//
// The loop in runWalStream interleaves I/O (streamAttempt, emit,
// sleep) with decision logic (permanent vs retryable, no-progress
// accounting, which backoff curve, when to stop). The decisions were
// only testable through a hand-mirrored simulator
// (retry_bounds_test.go's runRetryLoopSimulator) — and a mirror
// verifies the mirror: the simulator covered the setup-error half and
// silently omitted the whole mid-stream half (the issue-#45
// no-progress backstop, the duration-aware backoff reset, the
// draining-primary fast path). This state machine IS the loop's
// decision logic; the loop consults it and performs only I/O.
//
// Behaviour is preserved bit-for-bit from the inline original,
// including one deliberate asymmetry: the setup-failure path emits
// its reconnecting event with the PRE-escalation backoff and then
// escalates, while the stream-break path escalates first and emits
// the post-escalation value. Cosmetic, but pinned — changing emitted
// telemetry silently is how dashboards rot.

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/replication"
)

// retryAction is what the loop must do after a decision.
type retryAction int

const (
	// retryContinue: emit the reconnecting event (EmitBackoff, Reason)
	// and sleep SleepFor, then run the next attempt.
	retryContinue retryAction = iota
	// retryReturnSetupErr: return the attempt's setup error verbatim.
	retryReturnSetupErr
	// retryReturnStreamErr: wrap the stream error as wal.stream_error
	// (the --no-reconnect mid-stream exit).
	retryReturnStreamErr
	// retryReturnStop: return output.NewError(StopCode, StopMsg)
	// wrapping the stream error (decideStreamStop verdict).
	retryReturnStop
	// retryBreak: leave the loop for the clean-stop epilogue.
	retryBreak
)

// attemptOutcome is everything one streamAttempt tells the policy.
type attemptOutcome struct {
	SetupErr    error // non-nil: failed before streaming began
	StreamErr   error // mid-stream exit error (nil on clean end)
	Synced      pglogrepl.LSN
	AttemptWall time.Duration // how long the attempt lived
	CtxErr      error         // streamCtx.Err() observed after the attempt
}

// retryDecision carries the action plus what the loop needs to emit.
type retryDecision struct {
	Action      retryAction
	StopCode    string
	StopMsg     string
	Reason      string        // reconnecting event's reason field
	EmitBackoff time.Duration // value the event reports
	SleepFor    time.Duration // value the loop sleeps
}

// streamRetryPolicy holds the loop's decision state across attempts.
type streamRetryPolicy struct {
	noReconnect bool
	initial     time.Duration
	maxBackoff  time.Duration

	backoff         time.Duration
	noProgress      int
	lastProgressLSN pglogrepl.LSN
}

func newStreamRetryPolicy(noReconnect bool, initial, maxBackoff time.Duration) *streamRetryPolicy {
	return &streamRetryPolicy{
		noReconnect: noReconnect,
		initial:     initial,
		maxBackoff:  maxBackoff,
		backoff:     initial,
	}
}

// decide consumes one attempt's outcome and returns what the loop
// does next, mutating the policy's backoff / progress state exactly
// as the inline loop did.
func (p *streamRetryPolicy) decide(o attemptOutcome) retryDecision {
	if o.SetupErr != nil {
		// Pre-Stream setup error (preflight, ensureSlot,
		// resolveStartLSN, connect). Most setup failures clear once
		// the new leader is up — but permanent ones (operator must
		// intervene) bypass the loop, or a recycled-WAL gap surfaces
		// as a tight reconnecting loop masking the real structured
		// error (issue #79).
		if o.CtxErr != nil {
			return retryDecision{Action: retryBreak}
		}
		if p.noReconnect || isPermanentStreamSetupError(o.SetupErr) {
			return retryDecision{Action: retryReturnSetupErr}
		}
		d := retryDecision{
			Action:      retryContinue,
			Reason:      "setup_failure",
			EmitBackoff: p.backoff, // pre-escalation, as always emitted
			SleepFor:    p.backoff,
		}
		p.backoff = nextBackoff(p.backoff, p.maxBackoff)
		return d
	}

	if o.StreamErr == nil || errors.Is(o.StreamErr, context.Canceled) {
		// Clean exit via ctx cancellation (signal or --once).
		return retryDecision{Action: retryBreak}
	}
	if p.noReconnect {
		return retryDecision{Action: retryReturnStreamErr}
	}

	// No-progress backstop (issue #45): judge unclassifiable
	// permanent failures by OUTCOME — an attempt that ends in error
	// having synced nothing made no progress, and a run of those
	// means retrying is not working, whatever the cause.
	if o.Synced > p.lastProgressLSN {
		p.lastProgressLSN = o.Synced
		p.noProgress = 0
	} else {
		p.noProgress++
	}
	if code, msg, stop := decideStreamStop(o.StreamErr, p.noProgress); stop {
		return retryDecision{Action: retryReturnStop, StopCode: code, StopMsg: msg}
	}

	// A connection that stayed up long enough to stream resets the
	// backoff; one that broke almost immediately keeps escalating
	// (CPU-pathology audit #1). The draining-primary exit reconnects
	// promptly by design (issue #34).
	reason := "stream_break"
	if errors.Is(o.StreamErr, replication.ErrPrimaryDraining) {
		reason = "primary_draining"
	}
	p.backoff = nextStreamBreakBackoff(o.AttemptWall, p.backoff, p.initial, p.maxBackoff)
	return retryDecision{
		Action:      retryContinue,
		Reason:      reason,
		EmitBackoff: p.backoff, // post-escalation, as always emitted
		SleepFor:    p.backoff,
	}
}
