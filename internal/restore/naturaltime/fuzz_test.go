package naturaltime_test

// fuzz_test.go — the PITR time parser's data-safety invariants.
//
// A misparse here is the campaign's signature failure: confidently
// wrong, silently. Parse feeds recovery_target_time on five paths; a
// well-formed but WRONG time.Time restores to the wrong instant with
// no downstream layer able to catch it. These targets hunt the two
// outcomes that would actually lose data — a relative "ago" landing
// in the FUTURE (recovers past the incident), and an explicit-UTC
// input silently shifted by hours — plus crash-freedom and the
// total-function contract.

import (
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore/naturaltime"
)

var fixedNow = time.Date(2026, 5, 15, 14, 30, 0, 0, time.UTC)

// FuzzParse_TotalAndCrashFree: no input panics, and success always
// yields a usable (non-zero) instant — never (nil error, zero time).
func FuzzParse_TotalAndCrashFree(f *testing.F) {
	for _, s := range []string{
		"now", "5 minutes ago", "yesterday 9pm", "today 09:30",
		"2026-04-27 09:42 UTC", "2026-04-27T09:42:00Z", "", "   ",
		"999999999999 years ago", "13am", "24:00", "-3 hours ago",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got, err := naturaltime.Parse(s, fixedNow) // must never panic
		if err == nil && got.IsZero() {
			t.Fatalf("Parse(%q) returned nil error AND the zero time — a caller would arm "+
				"recovery_target_time to 0001-01-01, indistinguishable from no target", s)
		}
	})
}

// FuzzParse_RelativeNeverFuture: any accepted "<N> <unit> ago" must
// resolve to <= now. An overflow wrap turning "ago" into the future
// would recover PAST the point the operator asked for — data loss.
func FuzzParse_RelativeNeverFuture(f *testing.F) {
	f.Add(uint64(5), "minutes")
	f.Add(uint64(1<<63-1), "hours")
	f.Add(uint64(9999999999), "days")
	f.Fuzz(func(t *testing.T, n uint64, unit string) {
		s := formatN(n) + " " + unit + " ago"
		got, err := naturaltime.Parse(s, fixedNow)
		if err != nil {
			return // rejection (unknown unit / overflow) is correct
		}
		if got.After(fixedNow) {
			t.Fatalf("Parse(%q) resolved to %s, which is AFTER now (%s) — a 'ago' request "+
				"landed in the FUTURE, recovering past the operator's target", s, got, fixedNow)
		}
	})
}

func formatN(n uint64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// FuzzParse_ExplicitUTCNotShifted: an RFC3339 instant in UTC must
// parse back to the SAME instant — no silent local-zone shift (the
// #70 failure class, swept here across arbitrary valid timestamps).
func FuzzParse_ExplicitUTCNotShifted(f *testing.F) {
	f.Add(int64(1_777_000_000), 0)
	f.Add(int64(0), 500)
	f.Fuzz(func(t *testing.T, unix int64, nanos int) {
		// Bound to a sane range so we build a real RFC3339 string.
		if unix < -62135596800 || unix > 253402300799 {
			return
		}
		if nanos < 0 || nanos >= 1e9 {
			nanos = 0
		}
		want := time.Unix(unix, int64(nanos)).UTC()
		s := want.Format(time.RFC3339Nano)
		got, err := naturaltime.Parse(s, fixedNow)
		if err != nil {
			t.Fatalf("Parse(%q) — a UTC RFC3339 string we just formatted — errored: %v", s, err)
		}
		if !got.Equal(want) {
			t.Fatalf("Parse(%q) = %s, want %s (Δ=%s) — an explicit-UTC target was silently "+
				"shifted; recovery would land at the wrong instant",
				s, got, want, got.Sub(want))
		}
	})
}
