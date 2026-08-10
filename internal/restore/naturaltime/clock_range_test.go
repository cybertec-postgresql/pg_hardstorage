package naturaltime_test

// clock_range_test.go — a 12-hour clock has hours 1..12. Malformed
// hours must be REJECTED, not silently resolved to a wrong instant.
//
// Found by the fuzz pass's follow-up probe: "13am" resolved to 13:00
// (1pm) and "0am"/"0pm" to midnight/noon. The hazard is a plausible
// typo — an operator meaning 1am types 13am and gets a recovery
// target twelve hours off, silently. Loud rejection beats a
// confidently-wrong PITR target.

import (
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore/naturaltime"
)

func TestParse_MalformedTwelveHourClockRejected(t *testing.T) {
	now := time.Date(2026, 5, 15, 14, 30, 0, 0, time.UTC)
	for _, bad := range []string{
		"today 13am", "today 0am", "today 0pm",
		"yesterday 13am", "today 99am",
	} {
		if got, err := naturaltime.Parse(bad, now); err == nil {
			t.Errorf("Parse(%q) accepted a malformed 12-hour clock, resolving to %s — it must "+
				"reject, or a typo silently misdirects the recovery target", bad, got)
		}
	}
}

func TestParse_ValidTwelveHourClockUnchanged(t *testing.T) {
	now := time.Date(2026, 5, 15, 14, 30, 0, 0, time.UTC)
	for _, tc := range []struct {
		in       string
		wantHour int
	}{
		{"today 12am", 0},  // midnight
		{"today 12pm", 12}, // noon
		{"today 1am", 1},
		{"today 9pm", 21},
		{"today 11:30pm", 23},
	} {
		got, err := naturaltime.Parse(tc.in, now)
		if err != nil {
			t.Fatalf("Parse(%q) rejected a VALID 12-hour clock: %v", tc.in, err)
		}
		if got.Hour() != tc.wantHour {
			t.Errorf("Parse(%q) hour = %d, want %d", tc.in, got.Hour(), tc.wantHour)
		}
	}
}
