package cli

// wal_stream_retry_policy_test.go — the retry loop's decisions,
// tested on the REAL state machine.
//
// These cases replace retry_bounds_test.go's hand-mirrored simulator,
// which verified its own mirror: it covered the setup-error half and
// silently omitted the mid-stream half entirely — the issue-#45
// no-progress backstop, the duration-aware backoff reset, and the
// draining-primary fast path had no control-flow coverage at all.
// streamRetryPolicy.decide IS the loop's decision logic now
// (runWalStream consults it and performs only I/O), so what passes
// here is what the shipped loop does.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pglogrepl"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/replication"
)

func setupErr(code string) error {
	return output.NewError(code, "synthetic for retry-policy test").Wrap(output.ErrUsage)
}

// --- Setup-error half (ported from the simulator, now on the real machine).

func TestRetryPolicy_PermanentSetupErrorExitsImmediately(t *testing.T) {
	for _, code := range []string{
		"wal.start_before_slot_restart_lsn",
		"wal.slot_no_restart_lsn",
		"usage.bad_lsn",
		"usage.unaligned_lsn",
		"usage.bad_flag",
	} {
		t.Run(code, func(t *testing.T) {
			p := newStreamRetryPolicy(false, time.Second, time.Minute)
			d := p.decide(attemptOutcome{SetupErr: setupErr(code)})
			if d.Action != retryReturnSetupErr {
				t.Errorf("permanent code %q: action %v, want retryReturnSetupErr — the "+
					"issue-#79 spin masked exactly this structured error behind a tight "+
					"reconnecting loop", code, d.Action)
			}
		})
	}
}

func TestRetryPolicy_TransientSetupErrorBacksOffAndEscalates(t *testing.T) {
	p := newStreamRetryPolicy(false, time.Second, time.Minute)
	transient := output.NewError("connect.replication", "transient")

	d1 := p.decide(attemptOutcome{SetupErr: transient})
	if d1.Action != retryContinue || d1.Reason != "setup_failure" {
		t.Fatalf("transient setup error: %+v, want continue/setup_failure", d1)
	}
	// The event reports the PRE-escalation value (pinned asymmetry:
	// the stream-break path reports post-escalation).
	if d1.EmitBackoff != time.Second || d1.SleepFor != time.Second {
		t.Errorf("first backoff should be the initial 1s; got emit=%v sleep=%v",
			d1.EmitBackoff, d1.SleepFor)
	}
	d2 := p.decide(attemptOutcome{SetupErr: transient})
	if d2.SleepFor <= d1.SleepFor {
		t.Errorf("backoff did not escalate: %v then %v", d1.SleepFor, d2.SleepFor)
	}
}

func TestRetryPolicy_SetupErrorBackoffCapsAtMax(t *testing.T) {
	max := 50 * time.Millisecond
	p := newStreamRetryPolicy(false, time.Millisecond, max)
	transient := output.NewError("connect.replication", "transient")
	var last time.Duration
	for i := 0; i < 100; i++ {
		d := p.decide(attemptOutcome{SetupErr: transient})
		if d.SleepFor > max {
			t.Fatalf("backoff exceeded max at iteration %d: %v > %v", i, d.SleepFor, max)
		}
		last = d.SleepFor
	}
	if last != max {
		t.Errorf("backoff should settle at max=%v; got %v", max, last)
	}
}

func TestRetryPolicy_CanceledContextBreaksBeforeClassification(t *testing.T) {
	p := newStreamRetryPolicy(false, time.Second, time.Minute)
	// Even a PERMANENT setup error yields a clean break when the
	// context is already gone — shutdown must not masquerade as an
	// operator-actionable failure.
	d := p.decide(attemptOutcome{
		SetupErr: setupErr("usage.bad_flag"),
		CtxErr:   context.Canceled,
	})
	if d.Action != retryBreak {
		t.Errorf("canceled ctx during setup error: %v, want retryBreak", d.Action)
	}
}

func TestRetryPolicy_NoReconnectExitsOnFirstSetupError(t *testing.T) {
	p := newStreamRetryPolicy(true, time.Second, time.Minute)
	d := p.decide(attemptOutcome{SetupErr: output.NewError("connect.replication", "transient")})
	if d.Action != retryReturnSetupErr {
		t.Errorf("--no-reconnect setup error: %v, want retryReturnSetupErr", d.Action)
	}
}

// --- Mid-stream half (NEW coverage — the simulator never modeled it).

func streamBreak(msg string) error { return errors.New(msg) }

func TestRetryPolicy_StreamCancelBreaksCleanly(t *testing.T) {
	p := newStreamRetryPolicy(false, time.Second, time.Minute)
	d := p.decide(attemptOutcome{StreamErr: context.Canceled})
	if d.Action != retryBreak {
		t.Errorf("canceled stream: %v, want retryBreak (clean shutdown epilogue)", d.Action)
	}
}

func TestRetryPolicy_NoProgressRunStops(t *testing.T) {
	p := newStreamRetryPolicy(false, time.Millisecond, time.Minute)
	// Attempts that error having synced nothing new: the loop must
	// eventually STOP rather than re-stream the same LSN forever —
	// issue #45's failure shape was chunks accumulating with no
	// manifest until the kernel OOM-killed the process.
	var stopped bool
	var attempts int
	for i := 0; i < 100; i++ {
		attempts++
		d := p.decide(attemptOutcome{
			StreamErr:   streamBreak("backend: NotImplemented"), // uncodable raw error
			Synced:      pglogrepl.LSN(0x1000000),               // never advances
			AttemptWall: time.Millisecond,
		})
		if d.Action == retryReturnStop {
			stopped = true
			if d.StopCode == "" || d.StopMsg == "" {
				t.Errorf("stop verdict without code/msg: %+v", d)
			}
			break
		}
		if d.Action != retryContinue {
			t.Fatalf("unexpected action %v at attempt %d", d.Action, attempts)
		}
	}
	if !stopped {
		t.Fatal("100 no-progress attempts never stopped the loop — issue #45's " +
			"re-stream-forever shape is back")
	}
	if attempts < 2 {
		t.Errorf("stopped after %d attempt(s) — a single mid-stream break must retry, "+
			"or every Patroni failover kills the streamer", attempts)
	}
}

func TestRetryPolicy_ProgressResetsTheNoProgressRun(t *testing.T) {
	p := newStreamRetryPolicy(false, time.Millisecond, time.Minute)
	lsn := uint64(0x1000000)
	// Every attempt fails, but each syncs FURTHER — a lurching stream
	// behind a flapping network. That must retry indefinitely: it IS
	// making progress, and stopping it would abandon a recovering
	// deployment.
	for i := 0; i < 100; i++ {
		lsn += 0x10000
		d := p.decide(attemptOutcome{
			StreamErr:   streamBreak("flap"),
			Synced:      pglogrepl.LSN(lsn),
			AttemptWall: time.Millisecond,
		})
		if d.Action != retryContinue {
			t.Fatalf("progressing stream stopped at iteration %d (%v) — progress must "+
				"reset the no-progress run", i, d.Action)
		}
	}
}

func TestRetryPolicy_LongLivedAttemptResetsBackoff(t *testing.T) {
	p := newStreamRetryPolicy(false, time.Second, time.Minute)
	quick := attemptOutcome{StreamErr: streamBreak("flap"), AttemptWall: 10 * time.Millisecond}
	// Escalate via quick failures...
	var escalated time.Duration
	for i := 0; i < 4; i++ {
		lsnBump := pglogrepl.LSN(0x1000000 * (i + 1)) // keep progress, isolate backoff behaviour
		quick.Synced = lsnBump
		escalated = p.decide(quick).SleepFor
	}
	if escalated <= time.Second {
		t.Fatalf("fixture broken: backoff never escalated (%v)", escalated)
	}
	// ...then one attempt that streamed for a long time before
	// breaking: the next reconnect must start back at the floor — a
	// stream that ran healthily for hours and hit a failover deserves
	// a prompt reconnect, not the flap-era penalty.
	long := attemptOutcome{
		StreamErr:   streamBreak("failover"),
		Synced:      pglogrepl.LSN(0x100000000),
		AttemptWall: time.Hour,
	}
	d := p.decide(long)
	if d.SleepFor != time.Second {
		t.Errorf("after a long-lived attempt the backoff should reset to the 1s floor; "+
			"got %v (the CPU-pathology fix in reverse: slow recovery after healthy streams)", d.SleepFor)
	}
}

func TestRetryPolicy_DrainingPrimaryReconnectsPromptly(t *testing.T) {
	p := newStreamRetryPolicy(false, time.Second, time.Minute)
	d := p.decide(attemptOutcome{
		StreamErr:   replication.ErrPrimaryDraining,
		Synced:      pglogrepl.LSN(0x1000000),
		AttemptWall: time.Hour,
	})
	if d.Action != retryContinue || d.Reason != "primary_draining" {
		t.Errorf("draining primary: %+v, want continue/primary_draining (issue #34's "+
			"prompt-reconnect contract)", d)
	}
}

func TestRetryPolicy_NoReconnectMidStreamReturnsStreamErr(t *testing.T) {
	p := newStreamRetryPolicy(true, time.Second, time.Minute)
	d := p.decide(attemptOutcome{StreamErr: streamBreak("break"), AttemptWall: time.Second})
	if d.Action != retryReturnStreamErr {
		t.Errorf("--no-reconnect mid-stream: %v, want retryReturnStreamErr", d.Action)
	}
}
