package naturaltime_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore/naturaltime"
)

// pinned reference instant: 2026-04-28 14:21:08 UTC, a Tuesday.
var refUTC = time.Date(2026, 4, 28, 14, 21, 8, 0, time.UTC)

// reference in a non-UTC zone, for "today/yesterday" tests.
var refBerlin = time.Date(2026, 4, 28, 14, 21, 8, 0, mustLoc("Europe/Berlin"))

func mustLoc(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

func TestParse_Now(t *testing.T) {
	got, err := naturaltime.Parse("now", refUTC)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(refUTC) {
		t.Errorf("now: got %v, want %v", got, refUTC)
	}
	// case-insensitive
	if got, err := naturaltime.Parse("NOW", refUTC); err != nil || !got.Equal(refUTC) {
		t.Errorf("NOW: got %v err %v", got, err)
	}
	if got, err := naturaltime.Parse("  Now  ", refUTC); err != nil || !got.Equal(refUTC) {
		t.Errorf("padded Now: got %v err %v", got, err)
	}
}

func TestParse_Empty(t *testing.T) {
	_, err := naturaltime.Parse("", refUTC)
	if !errors.Is(err, naturaltime.ErrEmpty) {
		t.Errorf("empty input must return ErrEmpty; got %v", err)
	}
	_, err = naturaltime.Parse("   \t\n", refUTC)
	if !errors.Is(err, naturaltime.ErrEmpty) {
		t.Errorf("whitespace-only input must return ErrEmpty; got %v", err)
	}
}

func TestParse_Relative(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"5 minutes ago", 5 * time.Minute},
		{"1 minute ago", 1 * time.Minute},
		{"30 seconds ago", 30 * time.Second},
		{"30 secs ago", 30 * time.Second},
		{"30 sec ago", 30 * time.Second},
		{"30 s ago", 30 * time.Second},
		{"2 hours ago", 2 * time.Hour},
		{"2 hr ago", 2 * time.Hour},
		{"2 h ago", 2 * time.Hour},
		{"1 day ago", 24 * time.Hour},
		{"3 days ago", 3 * 24 * time.Hour},
		{"1 week ago", 7 * 24 * time.Hour},
		{"3 weeks ago", 3 * 7 * 24 * time.Hour},
		{"  5   minutes   ago  ", 5 * time.Minute}, // whitespace tolerance
		{"5 MINUTES AGO", 5 * time.Minute},         // case insensitivity
	}
	for _, c := range cases {
		got, err := naturaltime.Parse(c.in, refUTC)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		want := refUTC.Add(-c.want)
		if !got.Equal(want) {
			t.Errorf("%q: got %v, want %v", c.in, got, want)
		}
	}
}

func TestParse_Relative_Zero(t *testing.T) {
	got, err := naturaltime.Parse("0 minutes ago", refUTC)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(refUTC) {
		t.Errorf("0 minutes ago: got %v, want %v", got, refUTC)
	}
}

func TestParse_Relative_BadUnit(t *testing.T) {
	_, err := naturaltime.Parse("5 fortnights ago", refUTC)
	if err == nil {
		t.Fatal("expected unknown-unit error")
	}
	if !strings.Contains(err.Error(), "fortnights") {
		t.Errorf("error should name the bad unit; got %v", err)
	}
}

// TestParse_Relative_Overflow: a count so large that count*unit
// overflows int64 nanoseconds must be REJECTED, not silently wrapped.
// Pre-fix, "3000000 hours ago" wrapped to a date in the FUTURE (year
// 2268) with no error — a silently-wrong PITR target. The largest
// units (weeks/days) overflow at the smallest counts, so they're the
// realistic trigger for a fat-fingered count.
func TestParse_Relative_Overflow(t *testing.T) {
	for _, in := range []string{
		"3000000 hours ago",
		"9999999 weeks ago",
		"100000000 days ago",
		"9223372036854775807 seconds ago", // max int64 count
	} {
		got, err := naturaltime.Parse(in, refUTC)
		if err == nil {
			t.Errorf("%q: expected overflow error, got %v", in, got)
			continue
		}
		if !strings.Contains(err.Error(), "overflow") {
			t.Errorf("%q: error should mention overflow; got %v", in, err)
		}
	}
	// Large-but-representable counts must still work (and land in the
	// past, not wrap to the future).
	for _, in := range []string{"100000 hours ago", "1000 weeks ago", "500 days ago"} {
		got, err := naturaltime.Parse(in, refUTC)
		if err != nil {
			t.Errorf("%q: should parse: %v", in, err)
			continue
		}
		if !got.Before(refUTC) {
			t.Errorf("%q: %q should be before the reference, not in the future", in, got)
		}
	}
}

func TestParse_Yesterday_NoTime(t *testing.T) {
	got, err := naturaltime.Parse("yesterday", refUTC)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 4, 27, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("yesterday: got %v, want %v", got, want)
	}
}

func TestParse_Yesterday_With12HourClock(t *testing.T) {
	cases := []struct {
		in        string
		hour, min int
	}{
		{"yesterday 9pm", 21, 0},
		{"yesterday 9 pm", 21, 0},
		{"yesterday 9am", 9, 0},
		{"yesterday 12am", 0, 0},  // midnight
		{"yesterday 12pm", 12, 0}, // noon
		{"yesterday 9:30pm", 21, 30},
		{"yesterday 12:15am", 0, 15},
	}
	for _, c := range cases {
		got, err := naturaltime.Parse(c.in, refUTC)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		want := time.Date(2026, 4, 27, c.hour, c.min, 0, 0, time.UTC)
		if !got.Equal(want) {
			t.Errorf("%q: got %v, want %v", c.in, got, want)
		}
	}
}

func TestParse_Yesterday_With24HourClock(t *testing.T) {
	got, err := naturaltime.Parse("yesterday 21:00", refUTC)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 4, 27, 21, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParse_TodayInLocalZone(t *testing.T) {
	// In Berlin (UTC+2 CEST in late April), "today 9am" = 09:00 Berlin = 07:00 UTC.
	got, err := naturaltime.Parse("today 9am", refBerlin)
	if err != nil {
		t.Fatal(err)
	}
	loc := mustLoc("Europe/Berlin")
	want := time.Date(2026, 4, 28, 9, 0, 0, 0, loc).UTC()
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParse_AbsoluteFormats(t *testing.T) {
	cases := []struct {
		in   string
		want time.Time
	}{
		{"2026-04-27T09:42:00Z", time.Date(2026, 4, 27, 9, 42, 0, 0, time.UTC)},
		{"2026-04-27T09:42:00.123456789Z", time.Date(2026, 4, 27, 9, 42, 0, 123456789, time.UTC)},
		{"2026-04-27 09:42:00 UTC", time.Date(2026, 4, 27, 9, 42, 0, 0, time.UTC)},
		{"2026-04-27 09:42 UTC", time.Date(2026, 4, 27, 9, 42, 0, 0, time.UTC)},
		{"2026-04-27 09:42:00", time.Date(2026, 4, 27, 9, 42, 0, 0, time.UTC)},
		{"2026-04-27", time.Date(2026, 4, 27, 0, 0, 0, 0, time.UTC)},
		{"2026-04-27T09:42:00+02:00", time.Date(2026, 4, 27, 7, 42, 0, 0, time.UTC)},
		{"2026-04-27 09:42:00+02", time.Date(2026, 4, 27, 7, 42, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		got, err := naturaltime.Parse(c.in, refUTC)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if !got.Equal(c.want) {
			t.Errorf("%q: got %v, want %v", c.in, got, c.want)
		}
	}
}

func TestParse_RoundTrip_RFC3339(t *testing.T) {
	// Anything we successfully parse must render back to a stable
	// RFC3339 form whose re-parse equals the original.
	in := "2026-04-27T09:42:00Z"
	t1, err := naturaltime.Parse(in, refUTC)
	if err != nil {
		t.Fatal(err)
	}
	t2, err := naturaltime.Parse(t1.Format(time.RFC3339), refUTC)
	if err != nil {
		t.Fatal(err)
	}
	if !t1.Equal(t2) {
		t.Errorf("round-trip drift: %v vs %v", t1, t2)
	}
}

func TestParse_Garbage(t *testing.T) {
	cases := []string{
		"not a real time",
		"five minutes ago", // word-form numeral not supported
		"next Tuesday",
		"sometime",
		"yesterday banana",
	}
	for _, c := range cases {
		if _, err := naturaltime.Parse(c, refUTC); err == nil {
			t.Errorf("%q: expected error, got nil", c)
		}
	}
}

// TestParse_ZeroTimeRejected: a literal that resolves to the Go zero
// time (0001-01-01T00:00:00Z) must be REJECTED, not returned as a
// success. The zero time is the sentinel the whole restore pipeline
// uses for "no time target set" (Recovery.IsTargetSet,
// buildAutoConfBlock, validateExplicitBackupForTime), so accepting it
// would arm a recovery whose --to target is silently dropped → an
// end-of-WAL recovery at the wrong point in time with no error shown.
func TestParse_ZeroTimeRejected(t *testing.T) {
	for _, in := range []string{
		"0001-01-01",
		"0001-01-01 00:00:00",
		"0001-01-01T00:00:00Z",
		"0001-01-01 00:00:00+00",
		"0001-01-01 00:00:00 UTC",
	} {
		got, err := naturaltime.Parse(in, refUTC)
		if err == nil {
			t.Errorf("%q: expected error (zero-time target), got %v", in, got)
		}
	}
	// A one-second-later instant is fine — only the exact zero time is
	// special, and it can't be a real PITR target anyway.
	if _, err := naturaltime.Parse("0001-01-01T00:00:01Z", refUTC); err != nil {
		t.Errorf("0001-01-01T00:00:01Z should parse (non-zero): %v", err)
	}
}

// TestParse_NumericOffsetWithMinutes regression-locks issue #70 (A):
// before the fix, the layout list contained `-07` (hour-only) but
// not `-07:00`, so `+05:30` (IST as a numeric offset) was rejected
// while `+05` worked.  Half-hour and quarter-hour offsets are real
// (India, Nepal, Iran, Newfoundland, parts of Australia) and PG's
// recovery_target_time happily accepts them — the parser must too.
func TestParse_NumericOffsetWithMinutes(t *testing.T) {
	cases := []struct {
		in   string
		want time.Time
	}{
		// User's exact reproducer from issue #70.
		{"2026-05-11 20:50:10+05:30", time.Date(2026, 5, 11, 15, 20, 10, 0, time.UTC)},
		// Negative offset with minutes (Newfoundland).
		{"2026-05-11 20:50:10-03:30", time.Date(2026, 5, 12, 0, 20, 10, 0, time.UTC)},
		// Quarter-hour offset (Nepal +05:45).
		{"2026-05-11 20:50:10+05:45", time.Date(2026, 5, 11, 15, 5, 10, 0, time.UTC)},
		// No-colon variant (still numeric, unambiguous).
		{"2026-05-11 20:50:10+0530", time.Date(2026, 5, 11, 15, 20, 10, 0, time.UTC)},
		// Space-separated variant.
		{"2026-05-11 20:50:10 +05:30", time.Date(2026, 5, 11, 15, 20, 10, 0, time.UTC)},
		// HH:MM (no seconds) with offset.
		{"2026-05-11 20:50 +05:30", time.Date(2026, 5, 11, 15, 20, 0, 0, time.UTC)},
		// RFC3339-ish nested form.
		{"2026-05-11T20:50:10+05:30", time.Date(2026, 5, 11, 15, 20, 10, 0, time.UTC)},
		// Hour-only offset still works (regression guard for the
		// "fixed the new case but broke the old" failure mode).
		{"2026-05-11 20:50:10+05", time.Date(2026, 5, 11, 15, 50, 10, 0, time.UTC)},
	}
	for _, c := range cases {
		got, err := naturaltime.Parse(c.in, refUTC)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if !got.Equal(c.want) {
			t.Errorf("%q:\n  got  %v\n  want %v", c.in, got, c.want)
		}
	}
}

// TestParse_AmbiguousZoneAbbrRejected regression-locks issue #70 (B):
// before the fix, the layout list contained `MST` named-zone forms
// which let Go's time.Parse silently fall back to offset 0 when the
// abbreviation wasn't in its known set — so `2026-05-11 20:50:10 IST`
// parsed to 20:50:10Z instead of 15:20:10Z (a 5h30m silent corruption).
// Now the parser rejects ambiguous abbreviations with a pointer to
// the numeric form.
func TestParse_AmbiguousZoneAbbrRejected(t *testing.T) {
	cases := []string{
		"2026-05-11 20:50:10 IST",
		"2026-05-11 20:50:10 EST",
		"2026-05-11 20:50:10 CET",
		"2026-05-11 20:50:10 PST",
		"2026-05-11 20:50:10 CST", // China / Central US — classic ambiguous
		"2026-05-11 20:50 IST",
	}
	for _, c := range cases {
		_, err := naturaltime.Parse(c, refUTC)
		if err == nil {
			t.Errorf("%q: must reject ambiguous abbreviation (silent UTC fallback was bug #70)", c)
			continue
		}
		// Error message must point operators at the numeric form so
		// the fix is self-documenting.
		if !strings.Contains(err.Error(), "+05:30") &&
			!strings.Contains(err.Error(), "numeric offset") {
			t.Errorf("%q: error should mention the numeric-offset fix; got %v", c, err)
		}
	}
}

// TestParse_UTCAliasesStillAccepted makes sure the abbreviation-
// rejection doesn't punish the well-defined UTC aliases.  UTC, GMT,
// UT, and a trailing Z are all unambiguous and stay supported.
func TestParse_UTCAliasesStillAccepted(t *testing.T) {
	want := time.Date(2026, 5, 11, 20, 50, 10, 0, time.UTC)
	cases := []string{
		"2026-05-11 20:50:10 UTC",
		"2026-05-11 20:50:10 utc",
		"2026-05-11 20:50:10 GMT",
		"2026-05-11 20:50:10 UT",
		// HH:MM form
		"2026-05-11 20:50 UTC",
	}
	for _, c := range cases {
		got, err := naturaltime.Parse(c, refUTC)
		if err != nil {
			t.Errorf("%q: %v", c, err)
			continue
		}
		w := want
		if !strings.Contains(c, ":10") {
			w = time.Date(2026, 5, 11, 20, 50, 0, 0, time.UTC)
		}
		if !got.Equal(w) {
			t.Errorf("%q:\n  got  %v\n  want %v", c, got, w)
		}
	}
}

func TestParse_OutOfRangeClock(t *testing.T) {
	cases := []string{
		"yesterday 25:00",
		"yesterday 12:60",
		"yesterday 13pm",
	}
	for _, c := range cases {
		if _, err := naturaltime.Parse(c, refUTC); err == nil {
			t.Errorf("%q: expected error, got nil", c)
		}
	}
}
