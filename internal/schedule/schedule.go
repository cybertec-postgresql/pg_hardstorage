// Package schedule implements the in-process scheduler that drives
// recurring backup and retention work without an external cron.
//
// Three schedule shapes are supported in v0.1:
//
//   - "every <duration>"   — fixed-interval triggering (e.g. "6h", "30m").
//     Goes well with backup: "back up every 6h".
//
//   - "daily_at HH:MM"     — once per day at the operator's local
//     wall-clock HH:MM.
//
//   - "at <RFC3339>"       — one-shot at the given absolute instant.
//
// The grammar is intentionally narrow. A future release adds proper
// cron expressions via the robfig/cron import without changing the
// Schedule interface — Next(after) is the contract every shape
// honours.
//
// Engine semantics:
//
//   - Tasks fire serially per Engine. Two tasks cannot run
//     concurrently in the same agent — that's the safest default
//     given that backup and rotate operations on the same deployment
//     would race each other on the same repo prefix. A future
//     revision can introduce per-deployment scheduling lanes.
//
//   - On task error the Engine emits a structured event but does NOT
//     stop. A transient backup failure must not silently kill the
//     scheduler.
//
//   - LastRun is in-memory only in v0.1. On agent restart every task
//     is "due now" and runs immediately. Persistent scheduling lands
//     via pg_timetable integration — operators already running
//     PG (everyone) get a PG-native scheduler, audited via SQL, with
//     no embedded-DB sidecar.
package schedule

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Schedule is a stateless predictor: given an instant, what is the
// next instant at which this schedule fires?
//
// "Stateless" means we don't track LastRun here — that's the
// Engine's responsibility. A schedule must therefore answer Next(t)
// purely as a function of t and the schedule's own configuration.
type Schedule interface {
	// Next returns the next firing time strictly AFTER `after`.
	// Returning time.Time{} means "no further firings" (one-shot
	// schedules return zero once they've fired).
	Next(after time.Time) time.Time

	// Description is a one-line human-readable form, used in agent
	// status output and event bodies. Stable across versions.
	Description() string
}

// Spec is the YAML-loadable schedule declaration. Exactly one field
// must be set; Parse rejects ambiguous specs.
type Spec struct {
	// Every is a Go duration string ("6h", "30m", "1h30m"). Triggers
	// once per duration, anchored at the agent's start time.
	Every string `yaml:"every,omitempty" json:"every,omitempty"`

	// DailyAt is HH:MM in the operator's local time zone. Triggers
	// once per calendar day at that wall-clock instant.
	DailyAt string `yaml:"daily_at,omitempty" json:"daily_at,omitempty"`

	// At is an RFC3339 absolute instant. Triggers once.
	At string `yaml:"at,omitempty" json:"at,omitempty"`
}

// IsZero reports whether the spec has no field set.
func (s Spec) IsZero() bool {
	return s.Every == "" && s.DailyAt == "" && s.At == ""
}

// Parse converts s to a runnable Schedule. Rejects multiple fields,
// no fields, and malformed values.
func Parse(s Spec) (Schedule, error) {
	set := 0
	if s.Every != "" {
		set++
	}
	if s.DailyAt != "" {
		set++
	}
	if s.At != "" {
		set++
	}
	if set == 0 {
		return nil, errors.New("schedule: spec is empty (set one of every / daily_at / at)")
	}
	if set > 1 {
		return nil, errors.New("schedule: spec sets multiple shapes (every / daily_at / at) — only one allowed")
	}

	switch {
	case s.Every != "":
		d, err := time.ParseDuration(s.Every)
		if err != nil {
			return nil, fmt.Errorf("schedule: every %q: %w", s.Every, err)
		}
		if d < time.Second {
			return nil, fmt.Errorf("schedule: every %q: must be at least 1s", s.Every)
		}
		return Every{Interval: d}, nil

	case s.DailyAt != "":
		hh, mm, err := parseHHMM(s.DailyAt)
		if err != nil {
			return nil, fmt.Errorf("schedule: daily_at %q: %w", s.DailyAt, err)
		}
		return DailyAt{Hour: hh, Minute: mm, Loc: time.Local}, nil

	case s.At != "":
		t, err := time.Parse(time.RFC3339, s.At)
		if err != nil {
			return nil, fmt.Errorf("schedule: at %q (want RFC3339): %w", s.At, err)
		}
		return &Once{When: t}, nil
	}
	// Unreachable.
	return nil, errors.New("schedule: unparseable spec")
}

// parseHHMM accepts "HH:MM" with 0-23 / 0-59 ranges. Tight on input
// to keep the failure mode "your config has a typo" rather than
// "you got a different schedule than you thought."
func parseHHMM(s string) (int, int, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return 0, 0, errors.New("expected HH:MM")
	}
	h := atoiBounded(parts[0], 0, 23)
	m := atoiBounded(parts[1], 0, 59)
	if h < 0 || m < 0 {
		return 0, 0, errors.New("HH:MM out of range")
	}
	return h, m, nil
}

// atoiBounded returns the parsed int if it falls in [lo, hi], else -1.
// Tight — any non-digit character or out-of-range value yields -1.
func atoiBounded(s string, lo, hi int) int {
	if len(s) == 0 || len(s) > 3 {
		return -1
	}
	v := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return -1
		}
		v = v*10 + int(c-'0')
		if v > hi {
			return -1
		}
	}
	if v < lo {
		return -1
	}
	return v
}

// Every triggers at fixed intervals.
type Every struct {
	Interval time.Duration
}

// Next implements Schedule.
func (e Every) Next(after time.Time) time.Time {
	return after.Add(e.Interval)
}

// Description implements Schedule.
func (e Every) Description() string {
	return fmt.Sprintf("every %s", e.Interval)
}

// DailyAt triggers once per calendar day at HH:MM in Loc.
type DailyAt struct {
	Hour, Minute int
	Loc          *time.Location
}

// Next implements Schedule. The next firing is today's HH:MM if
// strictly after `after`; otherwise tomorrow's.
func (d DailyAt) Next(after time.Time) time.Time {
	loc := d.Loc
	if loc == nil {
		loc = time.Local
	}
	a := after.In(loc)
	candidate := time.Date(a.Year(), a.Month(), a.Day(), d.Hour, d.Minute, 0, 0, loc)
	if !candidate.After(after) {
		// Roll to the same wall-clock HH:MM on the NEXT CALENDAR DAY —
		// not candidate.Add(24h). A fixed 24h step drifts across DST:
		// a spring-forward day is 23h and a fall-back day 25h, so +24h
		// lands an hour off the intended local time (and stays off for
		// a day or two) around each transition. time.Date with day+1
		// applies tomorrow's correct local offset, so "daily at 02:30"
		// stays 02:30 local every day.
		candidate = time.Date(a.Year(), a.Month(), a.Day()+1, d.Hour, d.Minute, 0, 0, loc)
	}
	return candidate
}

// Description implements Schedule.
func (d DailyAt) Description() string {
	return fmt.Sprintf("daily at %02d:%02d (%s)", d.Hour, d.Minute, d.locName())
}

func (d DailyAt) locName() string {
	if d.Loc == nil {
		return "Local"
	}
	return d.Loc.String()
}

// Once triggers a single time at a specific instant. After it fires,
// Next returns the zero Time, signalling "no further firings."
type Once struct {
	When  time.Time
	fired sync.Once
	doneC chan struct{}
}

// Next implements Schedule. Returns zero once the firing window has
// passed (the engine treats zero as "remove this task").
func (o *Once) Next(after time.Time) time.Time {
	if o.When.After(after) {
		return o.When
	}
	return time.Time{}
}

// Description implements Schedule.
func (o *Once) Description() string {
	return fmt.Sprintf("once at %s", o.When.UTC().Format(time.RFC3339))
}
