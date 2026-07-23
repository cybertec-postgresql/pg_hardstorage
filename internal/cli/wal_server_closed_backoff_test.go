package cli

import (
	"testing"
	"time"
)

// Regression (issue #34): after the SERVER ends the COPY (walsender
// shutting down for a demote), the reconnect delay must NOT drop to the
// 1s floor — that re-armed a walsender on the still-up demoting primary
// every second and hung the switchover. It must sit at/above the grace
// floor and escalate.
func TestServerClosedBackoff(t *testing.T) {
	const max = 30 * time.Second
	// From the 1s floor, a server-closed reconnect jumps to at least
	// the grace floor (never stays at 1-2s).
	got := serverClosedBackoff(time.Second, max)
	if got < serverClosedGrace {
		t.Errorf("serverClosedBackoff(1s) = %v, want >= grace %v", got, serverClosedGrace)
	}
	// Escalates on repeat, capped at max.
	prev := got
	for i := 0; i < 6; i++ {
		next := serverClosedBackoff(prev, max)
		if next < prev && prev < max {
			t.Errorf("backoff went backwards: %v -> %v", prev, next)
		}
		if next > max {
			t.Errorf("backoff %v exceeds max %v", next, max)
		}
		prev = next
	}
	if prev != max {
		t.Errorf("backoff did not reach max after escalation: %v", prev)
	}
	// A low operator max is still respected (never exceeds it).
	if b := serverClosedBackoff(time.Second, 3*time.Second); b > 3*time.Second {
		t.Errorf("serverClosedBackoff exceeded low max: %v", b)
	}
}
