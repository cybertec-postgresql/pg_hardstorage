// duration_days.go — duration flags that accept the day and week units
// their own help text advertises.
package cli

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/pflag"
)

// ParseDurationDays parses a Go duration extended with `d` (days) and
// `w` (weeks).
//
// time.ParseDuration stops at hours, so `30d` is a parse error:
//
//	time: unknown unit "d" in duration "30d"
//
// Retention and forecast horizons are written in days by everyone who
// operates them, and this CLI's own help text says so — `--keep-for 30d`,
// `--horizon 365d`. Those examples did not work (issue #52): the flags
// were plain DurationVars, so the documented spelling was rejected while
// the equivalent `720h` was not. A flag whose first example fails is
// worse than one with no example.
//
// Days are 24h and weeks are 7 days. That is wall-clock arithmetic, not
// calendar arithmetic: it does not account for DST transitions or leap
// seconds. For retention windows and capacity horizons — "keep roughly a
// month", "project a quarter ahead" — that is the intended meaning, and
// a calendar-aware month would make `--keep-for` depend on WHEN it ran.
func ParseDurationDays(s string) (time.Duration, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return 0, fmt.Errorf("empty duration")
	}
	neg := false
	switch trimmed[0] {
	case '-':
		neg, trimmed = true, trimmed[1:]
	case '+':
		trimmed = trimmed[1:]
	}

	var total time.Duration
	rest := trimmed
	matched := false
	// Consume leading <number><d|w> groups, then hand whatever remains
	// to time.ParseDuration so "1w2d3h" works and "30m" is untouched.
	for {
		i := 0
		for i < len(rest) && (rest[i] >= '0' && rest[i] <= '9' || rest[i] == '.') {
			i++
		}
		if i == 0 || i >= len(rest) {
			break
		}
		unit := rest[i]
		if unit != 'd' && unit != 'w' {
			break
		}
		n, err := strconv.ParseFloat(rest[:i], 64)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		per := 24 * float64(time.Hour)
		if unit == 'w' {
			per *= 7
		}
		total += time.Duration(n * per)
		rest = rest[i+1:]
		matched = true
	}
	if rest != "" {
		d, err := time.ParseDuration(rest)
		if err != nil {
			if matched {
				return 0, fmt.Errorf("invalid duration %q: %w", s, err)
			}
			return 0, err
		}
		total += d
	}
	if neg {
		total = -total
	}
	return total, nil
}

// daysDurationValue adapts ParseDurationDays to pflag so `--help` still
// renders the flag as a duration.
type daysDurationValue struct{ target *time.Duration }

func (v daysDurationValue) String() string {
	if v.target == nil {
		return "0s"
	}
	return v.target.String()
}
func (v daysDurationValue) Set(s string) error {
	d, err := ParseDurationDays(s)
	if err != nil {
		return err
	}
	*v.target = d
	return nil
}
func (daysDurationValue) Type() string { return "duration" }

// DurationDaysVar registers a duration flag that accepts d/w units.
// Drop-in for pflag's DurationVar.
func DurationDaysVar(f *pflag.FlagSet, target *time.Duration, name string, value time.Duration, usage string) {
	*target = value
	f.Var(daysDurationValue{target: target}, name, usage)
}
