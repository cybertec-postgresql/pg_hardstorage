// Package naturaltime parses operator-friendly time expressions like
// "5 minutes ago", "yesterday 9pm", or "2026-04-27 09:42 UTC" into
// time.Time values usable as recovery_target_time.
//
// The parser is intentionally small and predictable. We do NOT vendor
// a general-purpose date-parsing library because:
//
//   - Every accepted expression must round-trip into a single
//     time.Time we can render as a PG GUC. Ambiguous inputs ("next
//     Tuesday") are forbidden by design.
//
//   - Surprises at 3am are unacceptable. An operator typing
//     `--to "5 minutes ago"` must trust the result. We accept a
//     small grammar and reject everything else with a clear error.
//
//   - The 24-month backward-compat commitment we hold for the JSON
//     schema applies to this parser too: a v0.1 expression must
//     parse to the same instant in v1.5.
//
// Grammar (case-insensitive, whitespace-tolerant):
//
//	now
//	N <unit> ago                         # e.g. 5 minutes ago, 1 hour ago
//	yesterday [HH:MM]                    # midnight if no time given
//	yesterday HHam|HHpm                  # 9am, 9pm, 12am
//	today [HH:MM]                        # in the operator's local time
//	today HHam|HHpm
//	YYYY-MM-DD                           # midnight UTC
//	YYYY-MM-DD HH:MM[:SS] [±HH:MM]       # numeric offsets only (±HH, ±HHMM, ±HH:MM)
//	YYYY-MM-DD HH:MM[:SS] UTC            # UTC / GMT / UT / Z aliases too
//	RFC3339 (e.g. 2026-04-27T09:42:00Z, 2026-04-27T09:42+05:30)
//
// Units: second, seconds, sec, secs, s; minute, minutes, min, mins, m;
//
//	hour, hours, hr, hrs, h; day, days, d; week, weeks, w.
//
// "today" / "yesterday" with a bare HH:MM use the operator's local
// time zone (not UTC) because that's what humans mean. The parser
// converts to UTC for storage. RFC3339 / explicit-numeric-offset
// inputs are honoured as given.
//
// Timezone abbreviations beyond UTC aliases (IST, EST, CET, …) are
// REJECTED rather than silently parsed as UTC.  Most three-letter
// abbreviations are ambiguous (IST = India / Irish / Israel; CST =
// Central / China; EST = US Eastern / Australian Eastern) and Go's
// time.Parse cannot resolve them — it falls back to offset 0, which
// gives the operator a recovery_target_time that is silently 5-12
// hours off the value they typed.  Numeric offsets (`+05:30`) are
// unambiguous; we require them.  See issue #70.
package naturaltime

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ErrEmpty is returned when the input is the empty string after
// trimming. We treat "" as a usage error rather than "now" so
// scripts that forgot a flag value get a loud signal.
var ErrEmpty = errors.New("naturaltime: empty input")

// Parse converts s to a time.Time using the small grammar described
// in the package doc. now is the reference for relative expressions
// ("5 minutes ago" relative to now); use time.Now() in production
// and pin it in tests for determinism. Returns the time in UTC.
//
// Parse never returns the Go zero time (0001-01-01T00:00:00Z) as a
// SUCCESS: that instant is both an absurd PITR target and — fatally —
// indistinguishable from "no target set", since the whole restore
// pipeline uses time.Time.IsZero() as the "is a time target present?"
// sentinel (Recovery.IsTargetSet, buildAutoConfBlock,
// validateExplicitBackupForTime, …). A literal "0001-01-01" parses
// cleanly to that instant, so without this guard `--to 0001-01-01`
// would arm a recovery whose target is silently dropped → end-of-WAL
// recovery (the issue-#99 silent-wrong-recovery class). Reject it
// with a clear usage error instead.
func Parse(s string, now time.Time) (time.Time, error) {
	t, err := parse(s, now)
	if err != nil {
		return time.Time{}, err
	}
	if t.IsZero() {
		return time.Time{}, fmt.Errorf(
			"naturaltime: %q resolves to the zero time (0001-01-01T00:00:00Z), which is not a valid recovery target and is indistinguishable from \"no target\"; use a real timestamp", s)
	}
	return t, nil
}

// parse is the grammar implementation; Parse wraps it with the
// zero-time guard described above.
func parse(s string, now time.Time) (time.Time, error) {
	in := strings.TrimSpace(s)
	if in == "" {
		return time.Time{}, ErrEmpty
	}
	low := strings.ToLower(in)

	// "now"
	if low == "now" {
		return now.UTC(), nil
	}

	// "<N> <unit> ago"
	if t, ok, err := parseRelative(low, now); ok {
		return t, err
	}

	// "yesterday [..]"
	if strings.HasPrefix(low, "yesterday") {
		return parseRelativeDay(low[len("yesterday"):], now.AddDate(0, 0, -1), now.Location())
	}

	// "today [..]"
	if strings.HasPrefix(low, "today") {
		return parseRelativeDay(low[len("today"):], now, now.Location())
	}

	// Strip a trailing zone abbreviation before trying the absolute
	// formats.  UTC aliases (UTC / GMT / UT / Z) are stripped and the
	// rest is parsed against UTC layouts.  Any OTHER trailing
	// alphabetic token is rejected up front with an explicit pointer
	// to the numeric form — without this guard, Go's time.Parse with
	// an `MST` layout would happily produce a fixed-offset-0 Location
	// named after the input (e.g. "IST" → +00:00) and the operator
	// would get a recovery_target_time silently off by hours.
	// Issue #70.
	stripped, namedUTC, badAbbr := splitTrailingZoneAbbr(in)
	if badAbbr != "" {
		return time.Time{}, fmt.Errorf(
			"naturaltime: timezone abbreviation %q is ambiguous (e.g. IST = India / Irish / Israel; CST = Central / China); use a numeric offset like +05:30 instead in %q",
			badAbbr, s)
	}
	in = stripped
	_ = namedUTC // accepted aliases parse as UTC via the default below

	// Try absolute formats — list ordered most-specific to
	// least-specific so RFC3339 doesn't accidentally swallow a
	// partial date.  Both `-07:00` and `-07` numeric-offset variants
	// are listed so `+05:30` and `+05` are equally accepted (issue
	// #70 — the previous layout list had no `-07:00` form).
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05.999999-07:00",
		"2006-01-02 15:04:05-07:00",
		"2006-01-02 15:04:05 -07:00",
		"2006-01-02 15:04 -07:00",
		"2006-01-02 15:04:05-0700",
		"2006-01-02 15:04 -0700",
		"2006-01-02 15:04:05.999999-07",
		"2006-01-02 15:04:05-07",
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006-01-02",
	} {
		if t, err := time.ParseInLocation(layout, in, time.UTC); err == nil {
			return t.UTC(), nil
		}
	}

	return time.Time{}, fmt.Errorf("naturaltime: cannot parse %q", s)
}

// trailingAbbrRe matches a space-separated trailing alphabetic token
// — that's the only shape Go's `MST` layout would have accepted, and
// the shape operators use when typing a named zone after the time.
// Numeric offsets like `+05:30` don't match (they aren't alphabetic),
// so they flow through to the layout list unmolested.
var trailingAbbrRe = regexp.MustCompile(`\s+([A-Za-z]+)\s*$`)

// splitTrailingZoneAbbr peels a trailing zone abbreviation off in
// and classifies it.  Returns:
//   - rest:    the input with the abbreviation removed (caller parses
//     this against UTC layouts when isUTC is true)
//   - isUTC:   the abbreviation is a UTC alias (UTC / GMT / UT / Z) —
//     the input maps unambiguously to UTC
//   - badAbbr: a non-empty string when the abbreviation is something
//     else (IST, EST, …); the caller surfaces a clear error
//
// If the input has no trailing alphabetic token, returns (in, false, "").
func splitTrailingZoneAbbr(in string) (rest string, isUTC bool, badAbbr string) {
	m := trailingAbbrRe.FindStringSubmatchIndex(in)
	if m == nil {
		return in, false, ""
	}
	abbr := in[m[2]:m[3]]
	switch strings.ToUpper(abbr) {
	case "UTC", "GMT", "UT", "Z":
		return strings.TrimSpace(in[:m[0]]), true, ""
	}
	return in, false, abbr
}

// relativeRe matches "<integer> <unit> ago" with flexible whitespace.
// We accept singular and plural unit forms; the unit is normalised in
// the canonical-unit table.
var relativeRe = regexp.MustCompile(`^\s*(\d+)\s+([a-z]+)\s+ago\s*$`)

// parseRelative handles the "N units ago" grammar. The third return
// is true when the input matched the regex (regardless of unit
// validity), so the caller knows whether to fall through.
func parseRelative(low string, now time.Time) (time.Time, bool, error) {
	m := relativeRe.FindStringSubmatch(low)
	if m == nil {
		return time.Time{}, false, nil
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return time.Time{}, true, fmt.Errorf("naturaltime: bad count %q: %w", m[1], err)
	}
	if n < 0 {
		return time.Time{}, true, fmt.Errorf("naturaltime: negative count %d", n)
	}
	d, derr := unitToDuration(m[2], n)
	if derr != nil {
		if errors.Is(derr, errUnknownUnit) {
			return time.Time{}, true, fmt.Errorf("naturaltime: unknown unit %q in %q", m[2], low)
		}
		// Overflow: the count is so large that the duration wraps int64
		// and would silently produce a wrong instant (often in the
		// FUTURE). Refuse loudly instead — a PITR target that far back
		// is never legitimate.
		return time.Time{}, true, fmt.Errorf("naturaltime: %q: %w", low, derr)
	}
	return now.Add(-d).UTC(), true, nil
}

// errUnknownUnit signals an unrecognised unit token (vs. an
// in-range-but-overflowing count, which is a different error).
var errUnknownUnit = errors.New("unknown unit")

// unitToDuration normalises a unit token + count into a Duration.
// Returns errUnknownUnit for unrecognised units, and a distinct
// overflow error when n * unit exceeds the int64 nanosecond range —
// without that check the multiplication wraps silently and
// `now.Add(-d)` yields a wrong instant, frequently in the FUTURE
// (e.g. "3000000 hours ago" → year 2268). Days and weeks use the
// 24h * 7 approximation — operators recovering "1 day ago" mean
// 24 hours, not "yesterday at this clock time."
func unitToDuration(unit string, n int) (time.Duration, error) {
	var per time.Duration
	switch unit {
	case "second", "seconds", "sec", "secs", "s":
		per = time.Second
	case "minute", "minutes", "min", "mins", "m":
		per = time.Minute
	case "hour", "hours", "hr", "hrs", "h":
		per = time.Hour
	case "day", "days", "d":
		per = 24 * time.Hour
	case "week", "weeks", "w":
		per = 7 * 24 * time.Hour
	default:
		return 0, errUnknownUnit
	}
	d := time.Duration(n) * per
	// Multiplication overflow check: if dividing back doesn't recover
	// n, the product wrapped. (n==0 is always fine.)
	if n != 0 && d/per != time.Duration(n) {
		return 0, fmt.Errorf("count %d %s overflows the representable time range", n, unit)
	}
	return d, nil
}

// parseRelativeDay handles the "today [time]" / "yesterday [time]"
// grammar. day is the calendar day (today's `now` or `now - 24h`);
// suffix is everything after the keyword. Empty suffix → midnight in
// loc; "9pm" / "21:00" / "9:30am" use loc as the wall-clock zone.
func parseRelativeDay(suffix string, day time.Time, loc *time.Location) (time.Time, error) {
	suffix = strings.TrimSpace(suffix)
	year, month, dayOfMonth := day.In(loc).Date()
	if suffix == "" {
		return time.Date(year, month, dayOfMonth, 0, 0, 0, 0, loc).UTC(), nil
	}
	hour, minute, ok := parseClock(suffix)
	if !ok {
		return time.Time{}, fmt.Errorf("naturaltime: cannot parse clock %q", suffix)
	}
	return time.Date(year, month, dayOfMonth, hour, minute, 0, 0, loc).UTC(), nil
}

// clockRe matches the bare clock forms we support after today/yesterday.
var (
	clock24Re = regexp.MustCompile(`^(\d{1,2}):(\d{2})$`)
	clock12Re = regexp.MustCompile(`^(\d{1,2})(?::(\d{2}))?\s*(am|pm)$`)
)

// parseClock extracts (hour, minute, ok). Accepts 24-hour "HH:MM" or
// 12-hour "Ham", "Hpm", "H:MMam", "H:MMpm". Out-of-range values yield
// ok=false.
func parseClock(s string) (int, int, bool) {
	if m := clock24Re.FindStringSubmatch(s); m != nil {
		h, _ := strconv.Atoi(m[1])
		mi, _ := strconv.Atoi(m[2])
		if h >= 0 && h < 24 && mi >= 0 && mi < 60 {
			return h, mi, true
		}
		return 0, 0, false
	}
	if m := clock12Re.FindStringSubmatch(s); m != nil {
		h, _ := strconv.Atoi(m[1])
		mi := 0
		if m[2] != "" {
			mi, _ = strconv.Atoi(m[2])
		}
		switch m[3] {
		case "am":
			if h == 12 {
				h = 0
			}
		case "pm":
			if h != 12 {
				h += 12
			}
		}
		if h >= 0 && h < 24 && mi >= 0 && mi < 60 {
			return h, mi, true
		}
	}
	return 0, 0, false
}
