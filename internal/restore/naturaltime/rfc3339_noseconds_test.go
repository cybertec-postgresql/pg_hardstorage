package naturaltime

import (
	"testing"
	"time"
)

// TestParse_RFC3339WithoutSeconds: the package doc advertises
// "2026-04-27T09:42+05:30" as accepted RFC3339 input. Go's RFC3339
// layout requires seconds, so the shape must be listed explicitly.
func TestParse_RFC3339WithoutSeconds(t *testing.T) {
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	cases := map[string]time.Time{
		"2026-04-27T09:42+05:30": time.Date(2026, 4, 27, 4, 12, 0, 0, time.UTC),
		"2026-04-27T09:42Z":      time.Date(2026, 4, 27, 9, 42, 0, 0, time.UTC),
		"2026-04-27T09:42-02:00": time.Date(2026, 4, 27, 11, 42, 0, 0, time.UTC),
		"2026-04-27T09:42":       time.Date(2026, 4, 27, 9, 42, 0, 0, time.UTC),
	}
	for in, want := range cases {
		got, err := Parse(in, now)
		if err != nil {
			t.Errorf("Parse(%q): %v", in, err)
			continue
		}
		if !got.Equal(want) {
			t.Errorf("Parse(%q) = %s, want %s", in, got, want)
		}
	}
}
