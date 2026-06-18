// schedule.go — Schedule: time-of-day bandwidth-cap windows (first-match-wins, UTC).
package throttle

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule encodes a set of time-of-day windows and the bandwidth
// cap that applies in each. The plan calls for "Egress shaping per
// repo per time-of-day. Bandwidth caps to avoid blowing through
// cloud-egress budget at month-end" — the per-time-of-day half
// lives here.
//
// Resolution model:
//
//   - The first matching window (in declaration order) wins.
//   - When no window matches the given time, DefaultBPS applies.
//   - DefaultBPS == 0 means "unbounded" — the throttle is a
//     transparent pass-through during the unmatched periods.
//
// Times are evaluated in UTC. Operators with local-time schedules
// must compute the UTC offsets themselves; we deliberately don't
// take a timezone parameter to keep the parser tiny + obvious.
type Schedule struct {
	Windows    []Window
	DefaultBPS int64
}

// Window is one (days, time-of-day, rate) tuple.
type Window struct {
	// Days is the set of weekdays this window covers. Nil/empty
	// means "every day".
	Days []time.Weekday

	// StartMinute / EndMinute are minutes-since-midnight (UTC),
	// half-open: [StartMinute, EndMinute). When EndMinute < StartMinute
	// the window wraps midnight (e.g. 22:00-06:00 covers
	// [22:00..24:00) and [00:00..06:00)).
	StartMinute int
	EndMinute   int

	// BPS is the bandwidth cap when this window is active. 0
	// means "unbounded" — the operator can declare a window that
	// explicitly reverts to unbounded inside an otherwise-capped
	// schedule (e.g. "Sun = unbounded for repo replicate").
	BPS int64
}

// BPSAt returns the active BPS at t. The first window that matches
// (by weekday + time-of-day) wins; otherwise DefaultBPS.
func (s *Schedule) BPSAt(t time.Time) int64 {
	if s == nil {
		return 0
	}
	tu := t.UTC()
	for _, w := range s.Windows {
		if w.MatchesAt(tu) {
			return w.BPS
		}
	}
	return s.DefaultBPS
}

// MatchesAt reports whether the window covers t (UTC implicit;
// caller passes a UTC time).
func (w *Window) MatchesAt(t time.Time) bool {
	// Day check.
	if len(w.Days) > 0 {
		wd := t.Weekday()
		ok := false
		for _, d := range w.Days {
			if d == wd {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	// Time-of-day check.
	mins := t.Hour()*60 + t.Minute()
	if w.EndMinute >= w.StartMinute {
		// Non-wrapping window.
		return mins >= w.StartMinute && mins < w.EndMinute
	}
	// Wrapping window: matches if before the end OR after the start.
	return mins >= w.StartMinute || mins < w.EndMinute
}

// ParseSchedule parses an operator-facing window expression.
//
// Grammar (spaces around tokens are tolerated):
//
//	expr   := window (";" window)*
//	window := [days ","] times "=" rate
//	days   := weekday ("-" weekday)?      // 3-letter, case-insensitive
//	times  := HH:MM "-" HH:MM             // 24-hour, UTC
//	rate   := "0" | <N> ("mbps"|"kbps")   // 0 = unbounded
//
// Examples:
//
//	"09:00-18:00=50mbps"
//	"Mon-Fri,09:00-18:00=50mbps"
//	"Mon-Fri,09:00-18:00=50mbps;Sat-Sun,00:00-23:59=200mbps"
//
// The DefaultBPS is left at 0 (unbounded outside any window). To
// declare a non-zero default, pass an "always" window like
// "00:00-23:59=10mbps" before the more specific windows.
func ParseSchedule(expr string) (*Schedule, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil, fmt.Errorf("throttle: empty schedule expression")
	}
	s := &Schedule{}
	for _, raw := range strings.Split(expr, ";") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		w, err := parseWindow(raw)
		if err != nil {
			return nil, fmt.Errorf("throttle: window %q: %w", raw, err)
		}
		s.Windows = append(s.Windows, w)
	}
	if len(s.Windows) == 0 {
		return nil, fmt.Errorf("throttle: schedule must have at least one window")
	}
	return s, nil
}

// parseWindow handles one comma-or-not "[days,]times=rate" string.
func parseWindow(raw string) (Window, error) {
	// Split on "=".
	eq := strings.LastIndex(raw, "=")
	if eq < 0 {
		return Window{}, fmt.Errorf("missing '=rate'")
	}
	left := strings.TrimSpace(raw[:eq])
	rateStr := strings.TrimSpace(raw[eq+1:])
	bps, err := parseRate(rateStr)
	if err != nil {
		return Window{}, err
	}

	// Left side is either "<times>" or "<days>,<times>".
	var daysPart, timesPart string
	if comma := strings.Index(left, ","); comma >= 0 {
		daysPart = strings.TrimSpace(left[:comma])
		timesPart = strings.TrimSpace(left[comma+1:])
	} else {
		timesPart = left
	}

	w := Window{BPS: bps}
	if daysPart != "" {
		days, err := parseDays(daysPart)
		if err != nil {
			return Window{}, err
		}
		w.Days = days
	}
	start, end, err := parseTimes(timesPart)
	if err != nil {
		return Window{}, err
	}
	w.StartMinute = start
	w.EndMinute = end
	return w, nil
}

// parseRate handles "0", "<N>mbps", "<N>kbps". Returns BPS in
// bytes-per-second.
func parseRate(s string) (int64, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "0" {
		return 0, nil
	}
	switch {
	case strings.HasSuffix(s, "mbps"):
		n, err := strconv.ParseFloat(strings.TrimSuffix(s, "mbps"), 64)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("rate %q: bad mbps value", s)
		}
		return int64(n * 1_000_000 / 8), nil
	case strings.HasSuffix(s, "kbps"):
		n, err := strconv.ParseFloat(strings.TrimSuffix(s, "kbps"), 64)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("rate %q: bad kbps value", s)
		}
		return int64(n * 1_000 / 8), nil
	}
	return 0, fmt.Errorf("rate %q: must be 0, <N>mbps, or <N>kbps", s)
}

// parseDays handles "Mon" or "Mon-Fri". Case-insensitive 3-letter.
// We don't allow comma-separated lists in a single window
// (operators with non-contiguous day sets declare multiple windows).
func parseDays(s string) ([]time.Weekday, error) {
	s = strings.TrimSpace(s)
	if dash := strings.Index(s, "-"); dash >= 0 {
		from, err := parseDay(s[:dash])
		if err != nil {
			return nil, err
		}
		to, err := parseDay(s[dash+1:])
		if err != nil {
			return nil, err
		}
		return weekdayRange(from, to), nil
	}
	d, err := parseDay(s)
	if err != nil {
		return nil, err
	}
	return []time.Weekday{d}, nil
}

func parseDay(s string) (time.Weekday, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "sun", "sunday":
		return time.Sunday, nil
	case "mon", "monday":
		return time.Monday, nil
	case "tue", "tues", "tuesday":
		return time.Tuesday, nil
	case "wed", "weds", "wednesday":
		return time.Wednesday, nil
	case "thu", "thur", "thurs", "thursday":
		return time.Thursday, nil
	case "fri", "friday":
		return time.Friday, nil
	case "sat", "saturday":
		return time.Saturday, nil
	}
	return 0, fmt.Errorf("unrecognised weekday %q", s)
}

// weekdayRange returns the inclusive weekday list from from to to,
// wrapping forward through the week. So Sat-Mon = [Sat, Sun, Mon].
func weekdayRange(from, to time.Weekday) []time.Weekday {
	out := []time.Weekday{from}
	if from == to {
		return out
	}
	cur := from
	for cur != to {
		cur = (cur + 1) % 7
		out = append(out, cur)
	}
	return out
}

// parseTimes handles "HH:MM-HH:MM". Returns start, end minutes.
func parseTimes(s string) (int, int, error) {
	s = strings.TrimSpace(s)
	dash := strings.Index(s, "-")
	if dash < 0 {
		return 0, 0, fmt.Errorf("times %q: missing '-'", s)
	}
	start, err := parseHHMM(s[:dash])
	if err != nil {
		return 0, 0, err
	}
	end, err := parseHHMM(s[dash+1:])
	if err != nil {
		return 0, 0, err
	}
	return start, end, nil
}

func parseHHMM(s string) (int, error) {
	s = strings.TrimSpace(s)
	colon := strings.Index(s, ":")
	if colon < 0 {
		return 0, fmt.Errorf("HH:MM %q: missing ':'", s)
	}
	h, err := strconv.Atoi(s[:colon])
	if err != nil || h < 0 || h > 23 {
		return 0, fmt.Errorf("HH:MM %q: bad hour", s)
	}
	m, err := strconv.Atoi(s[colon+1:])
	if err != nil || m < 0 || m > 59 {
		return 0, fmt.Errorf("HH:MM %q: bad minute", s)
	}
	return h*60 + m, nil
}

// String renders the schedule back to a parseable form. Round-trips
// through ParseSchedule (modulo whitespace + case).
func (s *Schedule) String() string {
	if s == nil {
		return ""
	}
	parts := make([]string, len(s.Windows))
	for i, w := range s.Windows {
		parts[i] = w.String()
	}
	return strings.Join(parts, ";")
}

// String renders the Window in the canonical "<days>,HH:MM-HH:MM=<rate>"
// form, matching what Parse accepts.
func (w *Window) String() string {
	var sb strings.Builder
	if len(w.Days) > 0 {
		sb.WriteString(formatDays(w.Days))
		sb.WriteString(",")
	}
	fmt.Fprintf(&sb, "%02d:%02d-%02d:%02d=%s",
		w.StartMinute/60, w.StartMinute%60,
		w.EndMinute/60, w.EndMinute%60,
		formatBPS(w.BPS))
	return sb.String()
}

func formatDays(days []time.Weekday) string {
	if len(days) == 0 {
		return ""
	}
	if len(days) == 1 {
		return shortDay(days[0])
	}
	// Detect contiguous range.
	contiguous := true
	for i := 1; i < len(days); i++ {
		if int(days[i]) != (int(days[i-1])+1)%7 {
			contiguous = false
			break
		}
	}
	if contiguous {
		return shortDay(days[0]) + "-" + shortDay(days[len(days)-1])
	}
	parts := make([]string, len(days))
	for i, d := range days {
		parts[i] = shortDay(d)
	}
	return strings.Join(parts, ",")
}

func shortDay(d time.Weekday) string {
	return [...]string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}[d]
}

func formatBPS(bps int64) string {
	if bps <= 0 {
		return "0"
	}
	mbps := float64(bps) * 8 / 1_000_000
	if mbps == float64(int64(mbps)) {
		return fmt.Sprintf("%dmbps", int64(mbps))
	}
	return fmt.Sprintf("%gmbps", mbps)
}
