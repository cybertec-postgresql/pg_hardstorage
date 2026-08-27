package cli

// wal_stream_retry_policy_property_test.go — properties of the
// reconnect loop's decision maker, driven by randomized attempt
// sequences.
//
// Item #4 of the coverage program. The existing policy tests assert
// examples; both timeline-resume bugs this cycle survived example
// tests and fell to properties (a termination assertion and a
// no-revisit assertion). The policy is pure, so its properties can be
// checked over thousands of random histories in milliseconds:
//
//   P1 TERMINATION WITHOUT PROGRESS — any infinite run of failing,
//      non-advancing attempts stops within maxNoProgressAttempts
//      decisions, whatever the error mix. This is the backstop that
//      turns "unclassifiable permanent failure" from an endless warm
//      loop into one actionable stop (issue #45).
//   P2 BOUNDED BACKOFF — the emitted backoff never exceeds the
//      configured maximum, no matter the attempt history.
//   P3 PERMANENT MEANS NOW — a permanent stream error is terminal on
//      the decision where it appears, never retried, regardless of
//      accumulated progress state.
//   P4 PROGRESS RESETS THE BACKSTOP — as long as synced advances at
//      least once every maxNoProgressAttempts-1 failures, the loop is
//      never stopped by the backstop. This is the property the
//      cross-timeline resume walk depends on: each reconnect commits
//      at least one whole segment (the walk's own tested invariant),
//      so a streamer catching up across many promotions must never be
//      killed as "no progress".

import (
	"errors"
	"math/rand"
	"testing"
	"time"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

var transientErr = errors.New("stream broke: connection reset")

func newTestPolicy() *streamRetryPolicy {
	return newStreamRetryPolicy(false, time.Second, 30*time.Second)
}

// The backstop counts STREAM attempts only. A retryable SETUP failure
// (connect refused — the database is down) is deliberately exempt: an
// HA streamer waits for its cluster the way any client daemon does,
// for hours if need be, and stopping it because the outage was long
// would turn every maintenance window into a dead archiver. The first
// draft of this property demanded termination for setup failures too
// and the policy correctly refused. Both halves of the real contract
// are pinned below.
func TestRetryPolicyProperty_NoStreamProgressAlwaysTerminates(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for trial := 0; trial < 2000; trial++ {
		p := newTestPolicy()
		stopped := false
		for step := 0; step < maxNoProgressAttempts+2; step++ {
			dec := p.decide(attemptOutcome{
				StreamErr:   transientErr,
				Synced:      0, // never advances
				AttemptWall: time.Duration(rng.Intn(120)) * time.Second,
			})
			if dec.Action == retryReturnStop {
				stopped = true
				break
			}
			if dec.Action != retryContinue {
				t.Fatalf("trial %d step %d: unexpected action %v", trial, step, dec.Action)
			}
		}
		if !stopped {
			t.Fatalf("trial %d: %d consecutive zero-progress stream failures and the policy is "+
				"still retrying — the issue-#45 backstop is not engaging", trial, maxNoProgressAttempts+2)
		}
	}
}

func TestRetryPolicyProperty_SetupFailuresWaitForTheClusterForever(t *testing.T) {
	const max = 30 * time.Second
	p := newStreamRetryPolicy(false, time.Second, max)
	for step := 0; step < 500; step++ {
		dec := p.decide(attemptOutcome{
			SetupErr:    errors.New("setup: connect refused"),
			AttemptWall: time.Second,
		})
		if dec.Action != retryContinue {
			t.Fatalf("step %d: a retryable setup failure stopped the loop (action=%v) — "+
				"a long outage would kill the archiver instead of waiting it out", step, dec.Action)
		}
		if dec.SleepFor > max {
			t.Fatalf("step %d: setup backoff %v exceeds max %v", step, dec.SleepFor, max)
		}
	}
}

func TestRetryPolicyProperty_BackoffNeverExceedsMax(t *testing.T) {
	const max = 30 * time.Second
	rng := rand.New(rand.NewSource(2))
	for trial := 0; trial < 500; trial++ {
		p := newStreamRetryPolicy(false, time.Second, max)
		synced := pglogrepl.LSN(0)
		for step := 0; step < 50; step++ {
			if rng.Intn(3) == 0 {
				synced += pglogrepl.LSN(1 << 24) // sometimes progress
			}
			dec := p.decide(attemptOutcome{
				StreamErr:   transientErr,
				Synced:      synced,
				AttemptWall: time.Duration(rng.Intn(90)) * time.Second,
			})
			if dec.Action != retryContinue {
				break
			}
			if dec.SleepFor > max || dec.EmitBackoff > max {
				t.Fatalf("trial %d step %d: backoff %v/%v exceeds the configured max %v",
					trial, step, dec.SleepFor, dec.EmitBackoff, max)
			}
		}
	}
}

func TestRetryPolicyProperty_PermanentErrorIsTerminalImmediately(t *testing.T) {
	permanent := storage.ErrUnsupported
	rng := rand.New(rand.NewSource(3))
	for trial := 0; trial < 500; trial++ {
		p := newTestPolicy()
		// Arbitrary healthy prefix: progressing, retryable attempts.
		synced := pglogrepl.LSN(0)
		for i, n := 0, rng.Intn(6); i < n; i++ {
			synced += pglogrepl.LSN(1 << 24)
			p.decide(attemptOutcome{StreamErr: transientErr, Synced: synced, AttemptWall: time.Minute})
		}
		dec := p.decide(attemptOutcome{StreamErr: permanent, Synced: synced, AttemptWall: time.Minute})
		if dec.Action != retryReturnStop {
			t.Fatalf("trial %d: permanent stream error was not terminal (action=%v) — "+
				"this is the endless-reconnect shape of issue #45", trial, dec.Action)
		}
	}
}

func TestRetryPolicyProperty_ProgressKeepsTheWalkAlive(t *testing.T) {
	// The cross-timeline resume walk reconnects once per promotion,
	// each attempt ending in an error (CopyDone / connection break)
	// but each committing at least one segment. However long the
	// chain, the backstop must never kill it.
	p := newTestPolicy()
	synced := pglogrepl.LSN(0)
	for step := 0; step < 200; step++ {
		synced += pglogrepl.LSN(16 << 20) // one whole segment per reconnect
		dec := p.decide(attemptOutcome{
			StreamErr:   transientErr,
			Synced:      synced,
			AttemptWall: 30 * time.Second,
		})
		if dec.Action != retryContinue {
			t.Fatalf("step %d: a walk that commits a segment every reconnect was stopped "+
				"(action=%v) — catching up across many promotions would be killed as "+
				"\"no progress\"", step, dec.Action)
		}
	}
}
