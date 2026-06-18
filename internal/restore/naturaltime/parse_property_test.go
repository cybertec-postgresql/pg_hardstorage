// parse_property_test.go — generative tests for naturaltime.Parse.
//
// Pins the ambiguous-timezone-rejection invariant from issue #70:
// every absolute time ending in an alphabetic timezone token other
// than the four UTC aliases (UTC / GMT / UT / Z) must be rejected
// loudly rather than silently parsed as UTC.
package naturaltime_test

import (
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore/naturaltime"
)

// refUTCProp is the reference instant for property tests.  Mid-year
// to avoid DST edge effects on the few zone-touching paths.
var refUTCProp = time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)

// Property 1: any RFC3339 timestamp parses, and re-formatting the
// result is byte-equal modulo timezone normalisation to UTC.
func TestProperty_Parse_RFC3339RoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate a random valid time within +/- 10 years of the
		// reference.  Time.Unix() gives us monotonic uniformity;
		// we then re-zone to UTC for canonical form.
		offset := rapid.Int64Range(-10*365*24*60*60, 10*365*24*60*60).Draw(t, "offset")
		want := refUTCProp.Add(time.Duration(offset) * time.Second).UTC()
		s := want.Format(time.RFC3339)
		got, err := naturaltime.Parse(s, refUTCProp)
		if err != nil {
			t.Fatalf("Parse(%q): %v", s, err)
		}
		if !got.Equal(want) {
			t.Errorf("round-trip mismatch: in=%q want=%v got=%v", s, want, got)
		}
	})
}

// Property 2: appending an alphabetic timezone abbreviation other
// than the four UTC aliases must result in an error.  Catches the
// "IST silently parsed as UTC" regression from #70.
func TestProperty_Parse_RejectsAmbiguousTimezoneAbbreviations(t *testing.T) {
	// Generated abbreviation set: any 3-4 letter alphabetic string
	// that ISN'T in the accept-list.
	utcAliases := map[string]bool{
		"UTC": true, "GMT": true, "UT": true, "Z": true,
	}
	rapid.Check(t, func(t *rapid.T) {
		abbr := rapid.StringMatching(`[A-Z]{3,4}`).Draw(t, "abbr")
		if utcAliases[abbr] {
			return // would legitimately parse — uninteresting
		}
		// Build a wall-clock string and tack the bad abbreviation on.
		s := "2026-05-11 20:50:10 " + abbr
		_, err := naturaltime.Parse(s, refUTCProp)
		if err == nil {
			t.Errorf("Parse(%q) returned no error — ambiguous TZ %q silently accepted (issue #70 regression)",
				s, abbr)
		}
	})
}

// Property 3: numeric +HH:MM offsets always parse correctly.
// Specifically pins the issue #70 fix that "+05:30" (IST) and
// "+05:45" (Nepal) are accepted alongside the basic "+HH".
func TestProperty_Parse_AcceptsNumericOffsets(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Hours [-12, +14], minutes ∈ {00, 15, 30, 45} — the real
		// offset set used by world timezones.
		hours := rapid.IntRange(-12, 14).Draw(t, "h")
		minutes := rapid.SampledFrom([]int{0, 15, 30, 45}).Draw(t, "m")
		sign := "+"
		if hours < 0 {
			sign = "-"
			hours = -hours
		}
		// Format consistently as +HH:MM.
		off := sign + pad2(hours) + ":" + pad2(minutes)
		s := "2026-05-11 20:50:10" + off
		got, err := naturaltime.Parse(s, refUTCProp)
		if err != nil {
			t.Errorf("Parse(%q) rejected legal numeric offset: %v", s, err)
			return
		}
		// Reconstruct the expected instant from the offset.
		zone := time.FixedZone("test", hoursToSec(sign, hours, minutes))
		want := time.Date(2026, 5, 11, 20, 50, 10, 0, zone).UTC()
		if !got.Equal(want) {
			t.Errorf("Parse(%q): got %v, want %v", s, got, want)
		}
	})
}

// Property 4: empty string and pure whitespace always error.
func TestProperty_Parse_RejectsEmptyOrWhitespace(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		ws := rapid.StringMatching(`[ \t\n\r]*`).Draw(t, "ws")
		_, err := naturaltime.Parse(ws, refUTCProp)
		if err == nil {
			t.Errorf("Parse(%q) returned no error on whitespace-only input", ws)
		}
	})
}

// Acceptance smoke: the issue #70 reproducer.
func TestParse_RejectsIST_AsAmbiguous(t *testing.T) {
	// Per issue #70: pg_hardstorage used to SILENTLY parse this as
	// UTC, shifting the restore target by 5h30m.  Must error.
	_, err := naturaltime.Parse("2026-05-11 20:50:10 IST", refUTCProp)
	if err == nil {
		t.Fatal("issue #70 regression: 'IST' was silently accepted as UTC")
	}
	if !strings.Contains(err.Error(), "IST") {
		t.Errorf("error %q does not mention the ambiguous abbreviation", err)
	}
}

// pad2 zero-pads to 2 digits.
func pad2(n int) string {
	if n < 10 {
		return "0" + itoa(n)
	}
	return itoa(n)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func hoursToSec(sign string, h, m int) int {
	s := h*3600 + m*60
	if sign == "-" {
		return -s
	}
	return s
}
