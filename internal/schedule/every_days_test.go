package schedule_test

// `every: 1d` is the natural way to write a daily backup schedule, and
// an operator who has learned from the docs that `keep_for: 30d` works
// will write it. Parse used time.ParseDuration, which rejects it with
// "unknown unit d" — the same failure as issue #54's keep_for, in the
// field right above it in the same config file.

import (
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/schedule"
)

func TestParse_EveryAcceptsDayAndWeekUnits(t *testing.T) {
	cases := []struct {
		every string
		want  time.Duration
	}{
		{"1d", 24 * time.Hour},
		{"7d", 7 * 24 * time.Hour},
		{"1w", 7 * 24 * time.Hour},
		{"6h", 6 * time.Hour}, // the documented spelling, unchanged
		{"30m", 30 * time.Minute},
	}
	for _, c := range cases {
		s, err := schedule.Parse(schedule.Spec{Every: c.every})
		if err != nil {
			t.Errorf("every %q: %v", c.every, err)
			continue
		}
		e, ok := s.(schedule.Every)
		if !ok {
			t.Fatalf("every %q: got %T, want schedule.Every", c.every, s)
		}
		if e.Interval != c.want {
			t.Errorf("every %q → %v, want %v", c.every, e.Interval, c.want)
		}
	}
}

// The sub-second floor and garbage rejection must survive the widening.
func TestParse_EveryStillRejectsBadValues(t *testing.T) {
	for _, bad := range []string{"1x", "abc", "500ms", "0d", "-1d"} {
		if _, err := schedule.Parse(schedule.Spec{Every: bad}); err == nil {
			t.Errorf("every %q was accepted", bad)
		}
	}
}
