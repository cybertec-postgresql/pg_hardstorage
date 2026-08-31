// duration_days.go — duration flags that accept the day and week units
// their own help text advertises.
package cli

import (
	"time"

	"github.com/spf13/pflag"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/durationx"
)

// ParseDurationDays parses a Go duration extended with `d` (days) and
// `w` (weeks).
//
// The implementation moved to internal/durationx so the YAML loader and
// internal/schedule can share it — issue #54: the docs advertise
// `keep_for: 30d` but the agent parsed that field with the stdlib and
// rejected every config it had told operators to write. This alias
// stays so the ~20 DurationDaysVar flag registrations keep reading
// naturally at their call sites.
func ParseDurationDays(s string) (time.Duration, error) {
	return durationx.Parse(s)
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
