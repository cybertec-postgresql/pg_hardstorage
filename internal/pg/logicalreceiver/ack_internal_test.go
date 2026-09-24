package logicalreceiver

import "testing"

// A keepalive's WAL end may be confirmed only while nothing handed to
// the sink is still unflushed — otherwise we would confirm past data
// the sink could still lose.
func TestAckTracker(t *testing.T) {
	var a ackTracker
	a.onKeepalive(100)
	if got := a.ack(0); got != 100 {
		t.Fatalf("idle keepalive not acknowledged: %d", got)
	}
	a.onRecord()
	a.onKeepalive(200) // record pending: must NOT advance
	if got := a.ack(150); got != 150 {
		t.Fatalf("acknowledged a keepalive past unflushed data: %d", got)
	}
	a.onFlushed()
	a.onKeepalive(300)
	if got := a.ack(150); got != 300 {
		t.Fatalf("after flush the keepalive must advance: %d", got)
	}
	if got := a.ack(400); got != 400 {
		t.Fatalf("sink ahead of keepalive must win: %d", got)
	}
}
