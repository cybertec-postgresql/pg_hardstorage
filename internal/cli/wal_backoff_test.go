package cli

import (
	"testing"
	"time"
)

// TestNextStreamBreakBackoff pins CPU-pathology audit #1: a stream that
// stayed up long enough to actually stream resets the reconnect backoff
// to the floor; one that broke almost immediately keeps ESCALATING so a
// flapping/instantly-failing stream can't spin in a tight full-setup
// reconnect loop.
func TestNextStreamBreakBackoff(t *testing.T) {
	const (
		initial = time.Second
		max     = 30 * time.Second
	)
	cases := []struct {
		name     string
		dur      time.Duration
		prev     time.Duration
		wantNext time.Duration
	}{
		{"healthy stream resets to floor", minHealthyStreamDuration, 16 * time.Second, initial},
		{"long stream resets to floor", time.Minute, 8 * time.Second, initial},
		{"instant fail escalates", 50 * time.Millisecond, time.Second, 2 * time.Second},
		{"repeated flap keeps doubling", 100 * time.Millisecond, 8 * time.Second, 16 * time.Second},
		{"escalation clamps at max", 10 * time.Millisecond, 30 * time.Second, max},
		{"just-under-threshold escalates", minHealthyStreamDuration - time.Millisecond, time.Second, 2 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := nextStreamBreakBackoff(tc.dur, tc.prev, initial, max)
			if got != tc.wantNext {
				t.Errorf("nextStreamBreakBackoff(%v, prev=%v) = %v, want %v", tc.dur, tc.prev, got, tc.wantNext)
			}
		})
	}
}

// TestNextStreamBreakBackoff_FlapDoesNotStayAtFloor: the core anti-spin
// property — N consecutive instant failures must drive the backoff UP,
// never staying pinned at the floor (which is the tight-loop bug).
func TestNextStreamBreakBackoff_FlapDoesNotStayAtFloor(t *testing.T) {
	const initial, max = time.Second, 30 * time.Second
	backoff := initial
	for i := 0; i < 6; i++ {
		backoff = nextStreamBreakBackoff(20*time.Millisecond, backoff, initial, max)
	}
	if backoff <= initial {
		t.Fatalf("after 6 instant flaps backoff = %v; must have escalated above the %v floor", backoff, initial)
	}
}
