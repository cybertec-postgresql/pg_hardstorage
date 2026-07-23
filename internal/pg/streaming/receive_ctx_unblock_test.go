package streaming_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/streaming"
)

// Regression (concurrency audit, demonstrated under -race): cancelling
// the PER-RECEIVE ctx must unblock a Receive parked in netConn.Read.
// The deadline-poking watcher is bound to the CONSTRUCTOR ctx, so a
// per-call recvCtx cancel (the status-tick failure path in
// replication.runReceiveLoop) previously did nothing — with a negative
// InactivityTimeout (the --no-inactivity-timeout / wal_sender_timeout=0
// posture) the stream hung forever: no status updates, restart_lsn
// frozen, unbounded WAL retention.
func TestReceive_PerCallCtxCancelUnblocks(t *testing.T) {
	ctx := context.Background() // constructor ctx: NEVER cancelled
	// Negative timeout = no read deadline — the exact hang posture.
	r, _, _ := pipeReader(t, ctx, streaming.Options{InactivityTimeout: -1})

	recvCtx, cancelRecv := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := r.Receive(recvCtx) // parks: the fake server sends nothing
		done <- err
	}()

	// Let Receive reach the blocking read, then cancel ONLY recvCtx.
	time.Sleep(100 * time.Millisecond)
	cancelRecv()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Receive returned %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Receive still blocked 3s after per-call ctx cancel (pre-fix hang)")
	}
}

// The cancel-before-the-deadline-set ordering must not be missable:
// a ctx cancelled between the loop-top check and the deadline set is
// caught by AfterFunc's run-immediately semantics.
func TestReceive_AlreadyCancelledCtxReturnsPromptly(t *testing.T) {
	r, _, _ := pipeReader(t, context.Background(), streaming.Options{InactivityTimeout: -1})
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() { _, err := r.Receive(cctx); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Receive returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Receive blocked on an already-cancelled ctx")
	}
}
