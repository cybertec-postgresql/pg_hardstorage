package replication

import (
	"context"
	"testing"
	"time"
)

func TestParseGUCDuration(t *testing.T) {
	for _, c := range []struct {
		in   string
		want time.Duration
	}{
		{"60s", time.Minute},
		{"5s", 5 * time.Second},
		{"1min", time.Minute},
		{"1500ms", 1500 * time.Millisecond},
		{"2h", 2 * time.Hour},
		{"1d", 24 * time.Hour},
		{"250", 250 * time.Millisecond}, // bare number: the GUC's base unit (ms)
		{"0", 0},
		{" 10s ", 10 * time.Second},
	} {
		got, err := parseGUCDuration(c.in)
		if err != nil || got != c.want {
			t.Errorf("parseGUCDuration(%q) = %v, %v; want %v", c.in, got, err, c.want)
		}
	}
	for _, bad := range []string{"", "s", "10 parsecs", "-5s"} {
		if _, err := parseGUCDuration(bad); err == nil {
			t.Errorf("parseGUCDuration(%q) accepted", bad)
		}
	}
}

// Issue #90: at the 10 s default against CloudNativePG's
// wal_sender_timeout of 5 s the primary terminated the walsender under
// load. The interval must land well inside the timeout.
func TestStatusIntervalFor(t *testing.T) {
	for _, c := range []struct {
		name                string
		configured, timeout time.Duration
		want                time.Duration
	}{
		{"pg default timeout keeps 10s", 10 * time.Second, 60 * time.Second, 10 * time.Second},
		{"cnpg 5s timeout", 10 * time.Second, 5 * time.Second, 5 * time.Second / 3},
		{"timeout disabled", 10 * time.Second, 0, 10 * time.Second},
		{"configured already shorter", time.Second, 60 * time.Second, time.Second},
		{"tiny timeout floored", 10 * time.Second, 30 * time.Millisecond, minStatusInterval},
	} {
		if got := statusIntervalFor(c.configured, c.timeout); got != c.want {
			t.Errorf("%s: statusIntervalFor(%v, %v) = %v; want %v", c.name, c.configured, c.timeout, got, c.want)
		}
		if c.timeout > minStatusInterval*3 {
			if got := statusIntervalFor(c.configured, c.timeout); got >= c.timeout/2 {
				t.Errorf("%s: interval %v is not inside half the timeout %v", c.name, got, c.timeout)
			}
		}
	}
}

// While the sink applies backpressure (OnRecord blocked because the
// upload pipeline is full) the receive side reads nothing, so PG's
// reply-requested keepalives go unseen. Status updates must keep
// flowing from the ticker anyway, or wal_sender_timeout fires.
func TestRunReceiveLoop_StatusUpdatesFlowWhileSinkBlocked(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pp := newPipePair(t, ctx)

	release := make(chan struct{})
	blocked := make(chan struct{})
	sink := &recordingSink{afterRecord: func() {
		close(blocked)
		<-release // hold the receive goroutine, as a full pipeline would
	}}

	statusUpdates := make(chan []byte, 64)
	go consumeServerWrites(pp.serverConn, statusUpdates)
	go func() {
		emitCopyData(t, pp.sendBackend, encodeXLogData(0x1000, 0x2000, 0, make([]byte, 64)))
	}()

	done := make(chan error, 1)
	go func() { done <- runReceiveLoop(ctx, pp.reader, sink, 20*time.Millisecond) }()

	<-blocked
	drainStatusUpdates(statusUpdates, 5*time.Millisecond) // discard the initial update
	n := drainStatusUpdates(statusUpdates, 150*time.Millisecond)
	close(release)
	cancel()
	<-done
	if n < 3 {
		t.Fatalf("only %d status updates in 150 ms while the sink was blocked (interval 20 ms)", n)
	}
}
