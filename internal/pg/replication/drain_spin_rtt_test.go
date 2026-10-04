package replication

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
)

// pingPongShutdown plays a shutting-down walsender: a caught-up,
// reply-requested keepalive, then wait for the client's reply, then
// wait rtt, and again — the real shutdown spin is this ping-pong, so its
// rate is one keepalive per network round trip.
func pingPongShutdown(ctx context.Context, pp *pipePair, rtt time.Duration) {
	replies := make(chan []byte, 64)
	go consumeServerWrites(pp.serverConn, replies)
	for ctx.Err() == nil {
		pp.sendBackend.Send(&pgproto3.CopyData{Data: encodeKeepalive(0, 0, true)})
		if err := pp.sendBackend.Flush(); err != nil {
			return
		}
		select {
		case <-replies:
		case <-ctx.Done():
			return
		}
		time.Sleep(rtt)
	}
}

// Issue #69: the issue-#34 drain detector required 200 keepalives within
// 2 s, and never reset its window. On localhost the spin runs at
// thousands per second; across subnets it runs at one per round trip, so
// a 10 ms+ RTT never reached 200 in 2 s — the streamer kept answering,
// the walsender kept waiting, and the Patroni demote hung until the
// streamer was restarted. Production defaults, a 20 ms round trip.
func TestRunReceiveLoop_ShutdownSpinDetectedAcrossSlowLink(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pp := newPipePair(t, ctx)
	sink := &recordingSink{}
	sink.syncedLSN.Store(0x5000000)

	go pingPongShutdown(ctx, pp, 20*time.Millisecond)

	done := make(chan error, 1)
	go func() { done <- runReceiveLoop(ctx, pp.reader, sink, time.Hour) }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrPrimaryDraining) {
			t.Fatalf("runReceiveLoop = %v, want ErrPrimaryDraining", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("shutdown spin over a 20 ms link not detected in 30 s — the demote would hang")
	}
}

// A healthy idle primary pings far less often: one reply-requested
// keepalive per wal_sender_timeout/2, i.e. seconds apart even for an
// aggressive timeout. Two pings a second for longer than a detection
// window must not read as a shutdown.
func TestRunReceiveLoop_FrequentHealthyKeepalivesNotDraining(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pp := newPipePair(t, ctx)
	sink := &recordingSink{}
	go pingPongShutdown(ctx, pp, 500*time.Millisecond)

	done := make(chan error, 1)
	go func() { done <- runReceiveLoop(ctx, pp.reader, sink, time.Hour) }()
	select {
	case err := <-done:
		t.Fatalf("a healthy 2/s keepalive cadence was mistaken for a shutdown: %v", err)
	case <-time.After(drainSpinWindow + 3*time.Second):
		cancel()
		<-done
	}
}

func shrinkDrainWindow(t *testing.T, win time.Duration, n int) {
	t.Helper()
	ow, on := drainSpinWindow, drainSpinKeepalives
	drainSpinWindow, drainSpinKeepalives = win, n
	t.Cleanup(func() { drainSpinWindow, drainSpinKeepalives = ow, on })
}

// The window must slide. A keepalive that opened a window long before
// the shutdown must not make the spin that follows undetectable — the
// one-shot window kept counting against its stale start and never fired.
func TestRunReceiveLoop_SpinAfterAnAgedWindowIsDetected(t *testing.T) {
	shrinkDrainWindow(t, time.Second, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pp := newPipePair(t, ctx)
	sink := &recordingSink{}
	go func() {
		replies := make(chan []byte, 64)
		go consumeServerWrites(pp.serverConn, replies)
		ping := func() bool {
			pp.sendBackend.Send(&pgproto3.CopyData{Data: encodeKeepalive(0, 0, true)})
			if pp.sendBackend.Flush() != nil {
				return false
			}
			select {
			case <-replies:
				return true
			case <-ctx.Done():
				return false
			}
		}
		for i := 0; i < 3; i++ { // a few healthy pings open a window...
			ping()
		}
		time.Sleep(1500 * time.Millisecond) // ...which then ages out
		for ctx.Err() == nil && ping() {    // then the shutdown spin
			time.Sleep(20 * time.Millisecond)
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
		t.Fatal("a spin starting after an aged window was never detected")
	}
}

// Healthy keepalives must not accumulate across windows into a false
// drain: 5 per 1 s window for 3 s stays under 10 per window.
func TestRunReceiveLoop_HealthyKeepalivesDoNotAccumulateAcrossWindows(t *testing.T) {
	shrinkDrainWindow(t, time.Second, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pp := newPipePair(t, ctx)
	sink := &recordingSink{}
	go pingPongShutdown(ctx, pp, 200*time.Millisecond)
	done := make(chan error, 1)
	go func() { done <- runReceiveLoop(ctx, pp.reader, sink, time.Hour) }()
	select {
	case err := <-done:
		t.Fatalf("healthy keepalives across windows read as a shutdown: %v", err)
	case <-time.After(3500 * time.Millisecond):
		cancel()
		<-done
	}
}
