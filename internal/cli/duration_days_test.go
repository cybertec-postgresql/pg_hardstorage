package cli

// duration_days_test.go — the day/week units the help text promises.
//
// `--keep-for 30d`, `--horizon 365d` and `--keep-since 14d` were all
// documented spellings that time.ParseDuration rejects ("unknown unit
// \"d\""), so each flag's own first example failed. Reported as part of
// issue #52; `--keep-since` had gone further and told the operator to
// convert by hand ("e.g. 14d → 336h").

import (
	"testing"
	"time"
)

func TestParseDurationDays_Units(t *testing.T) {
	day := 24 * time.Hour
	for _, tc := range []struct {
		in   string
		want time.Duration
	}{
		{"30d", 30 * day},
		{"1w", 7 * day},
		{"2w", 14 * day},
		{"365d", 365 * day},
		{"1w2d", 9 * day},
		{"1d12h", day + 12*time.Hour},
		{"0.5d", 12 * time.Hour},
		{"-7d", -7 * day},
		// Plain Go durations must keep working untouched.
		{"336h", 336 * time.Hour},
		{"90m", 90 * time.Minute},
		{"1h30m", time.Hour + 30*time.Minute},
		{"2s", 2 * time.Second},
	} {
		got, err := ParseDurationDays(tc.in)
		if err != nil {
			t.Errorf("ParseDurationDays(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseDurationDays(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// The documented example from the help text of each affected flag.
func TestParseDurationDays_DocumentedExamplesWork(t *testing.T) {
	for _, in := range []string{"30d", "365d", "90d", "14d", "7d"} {
		if _, err := ParseDurationDays(in); err != nil {
			t.Errorf("the help text offers %q but it does not parse: %v", in, err)
		}
	}
}

func TestParseDurationDays_RejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "d", "30x", "abc", "30dd", "1w2"} {
		if got, err := ParseDurationDays(in); err == nil {
			t.Errorf("ParseDurationDays(%q) = %v, want an error", in, got)
		}
	}
}

// A month is not a calendar month: keeping it wall-clock means
// `--keep-for 30d` retains the same window whenever it runs.
func TestParseDurationDays_DaysAreWallClock(t *testing.T) {
	got, err := ParseDurationDays("1d")
	if err != nil {
		t.Fatal(err)
	}
	if got != 24*time.Hour {
		t.Errorf("1d = %v, want exactly 24h", got)
	}
}
