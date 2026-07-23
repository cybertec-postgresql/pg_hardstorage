package replication

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
)

// Regression (issue #34): when the primary is shutting down it busy-loops
// caught-up keepalives waiting for a flush we cannot advance (the
// shutdown checkpoint lands in a partial segment). runReceiveLoop must
// detect the spin and return ErrPrimaryDraining so the walsender can
// exit and we reconnect to the new primary — instead of spinning
// forever and blocking the demote.
func TestRunReceiveLoop_ShutdownSpinReturnsDraining(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pp := newPipePair(t, ctx)
	sink := &recordingSink{}
	sink.syncedLSN.Store(0x5000000) // flush stuck at end of last full segment

	// The in-memory net.Pipe is far slower than a real TCP walsender
	// spin, so widen the detection window for the test (the threshold
	// count is what we exercise).
	origWin, origN := drainSpinWindow, drainSpinKeepalives
	drainSpinWindow = 30 * time.Second
	drainSpinKeepalives = 10
	t.Cleanup(func() { drainSpinWindow = origWin; drainSpinKeepalives = origN })

	// net.Pipe is synchronous — drain the loop's status-update writes.
	go drainServerWrites(pp.serverConn)

	// Server floods caught-up keepalives (serverEnd == 0, i.e. <= our
	// written) with no XLogData — the shutdown-spin signature. Raw send
	// (not emitCopyData) so the goroutine never touches t after the loop
	// returns early and closes the pipe; it just exits on write error.
	go func() {
		for i := 0; i < 1000; i++ {
			pp.sendBackend.Send(&pgproto3.CopyData{Data: encodeKeepalive(0, 0, false)})
			if err := pp.sendBackend.Flush(); err != nil {
				return
			}
		}
	}()

	done := make(chan error, 1)
	go func() { done <- runReceiveLoop(ctx, pp.reader, sink, time.Hour) }()

	select {
	case err := <-done:
		if !errors.Is(err, ErrPrimaryDraining) {
			t.Fatalf("runReceiveLoop = %v, want ErrPrimaryDraining", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runReceiveLoop did not detect the shutdown spin (would hang the demote)")
	}
}

// A slow, normal keepalive cadence with interleaved XLogData must NOT be
// mistaken for a shutdown spin.
func TestRunReceiveLoop_NormalKeepalivesNotDraining(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pp := newPipePair(t, ctx)
	sink := &recordingSink{}
	go drainServerWrites(pp.serverConn)

	go func() {
		// A handful of caught-up keepalives spaced out, with XLogData
		// between — far below the burst threshold within the window.
		for i := 0; i < 5; i++ {
			emitCopyData(t, pp.sendBackend, encodeXLogData(uint64(i*100), uint64(i*100+100), 0, []byte("wal")))
			emitCopyData(t, pp.sendBackend, encodeKeepalive(uint64(i*100+100), 0, false))
			time.Sleep(20 * time.Millisecond)
		}
	}()

	done := make(chan error, 1)
	go func() { done <- runReceiveLoop(ctx, pp.reader, sink, time.Hour) }()

	select {
	case err := <-done:
		t.Fatalf("runReceiveLoop returned early on a healthy stream: %v", err)
	case <-time.After(400 * time.Millisecond):
		// Good — still streaming, no false drain.
		cancel()
		<-done
	}
}
