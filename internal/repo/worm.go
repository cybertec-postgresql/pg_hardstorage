// worm.go — WORMPolicy: per-object retention deadline propagated to storage Object-Lock at PUT.
package repo

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// WORMPolicy declares a repository's write-once-read-many posture.
// When set on a repo's Metadata at init time, every committed
// object — chunks, manifests, replicas, audit events — gets a
// retention deadline propagated to the storage backend at PUT time
// via PutOptions.RetainUntil + ObjectLockMode.
//
// Once an object is committed under a WORM policy, the storage
// backend (S3 Object Lock, Azure immutable blob, etc.) refuses
// deletion until the retention deadline. In Compliance mode, even
// root credentials cannot delete the object — the regulatory
// posture the plan calls out for SEC-17a-4(f) / FINRA / etc.
//
// What WORM is NOT:
//
//   - Repository-wide read-only mode. That's `Mode = ModeReadOnly`
//     (see setmode.go); WORM is per-object retention enforced by
//     the backend.
//   - A retention CALCULATOR. WORM holds the floor — `keep_wal_days`
//     and `rotate`'s GFS retention can extend retention; they
//     can't shorten it below the WORM deadline.
//   - A delete enforcer. Soft-delete + tombstones still work for
//     visibility (a tombstoned manifest disappears from `list`);
//     the bytes themselves stay until the retention expires.
type WORMPolicy struct {
	// Mode selects the retention enforcement posture.
	//
	//   - "compliance" (recommended for regulated workloads): even
	//     root credentials can't override the retention.
	//   - "governance": IAM principals with the `BypassGovernance`
	//     permission can delete; everyone else cannot.
	//   - "" (empty): WORM disabled.
	Mode string `json:"mode"`

	// Retention is the operator-supplied duration string, preserved
	// verbatim for round-trip + auditability ("7y", "30d", "8760h").
	// Always normalized to RetentionSeconds at parse time.
	Retention string `json:"retention"`

	// RetentionSeconds is the resolved retention duration in seconds.
	// Computed from Retention at write time so reads don't have to
	// re-parse the operator-supplied form.
	RetentionSeconds int64 `json:"retention_seconds"`
}

// MaxRetentionSeconds is the largest retention this type can represent.
//
// RetainUntil computes now.Add(RetentionSeconds * time.Second), and
// time.Duration is int64 NANOSECONDS -- so it saturates at roughly 292
// years. Past that the multiplication wraps and the deadline lands in
// the PAST: "293y" produced 1734-12-04 and "1000y" produced 1856-11-25.
// A backend handed an already-expired ObjectLockRetainUntilDate either
// rejects the PUT or accepts it as expired, so an operator who asked to
// keep data effectively forever got NO WORM protection, silently, on
// exactly the repositories where that matters most.
//
// int64 nanoseconds / 1e9 ns-per-second = 9223372036 seconds.
const MaxRetentionSeconds = int64(math.MaxInt64) / int64(time.Second)

// IsZero reports whether p is unconfigured (Mode empty).
func (p *WORMPolicy) IsZero() bool {
	return p == nil || p.Mode == ""
}

// RetainUntil returns the retention deadline for an object PUT at
// `now` under this policy. Returns the zero time when the policy
// is unconfigured.
func (p *WORMPolicy) RetainUntil(now time.Time) time.Time {
	if p.IsZero() || p.RetentionSeconds <= 0 {
		return time.Time{}
	}
	secs := p.RetentionSeconds
	if secs > MaxRetentionSeconds {
		// CLAMP, do not overflow. Validate refuses such a value now, but
		// RetentionSeconds is persisted in repo metadata and read back on
		// every PUT, so a repository initialised before this limit was
		// enforced still carries one. Clamping yields the longest
		// representable protection; overflowing yields a deadline in the
		// past, which is no protection at all. Of the two ways to be
		// wrong, only one keeps the bytes.
		secs = MaxRetentionSeconds
	}
	return now.Add(time.Duration(secs) * time.Second).UTC()
}

// Validate checks that the policy is internally consistent. Used
// at init-time to reject malformed configurations early.
func (p *WORMPolicy) Validate() error {
	if p == nil {
		return nil
	}
	if p.Mode == "" && p.Retention == "" && p.RetentionSeconds == 0 {
		return nil // unconfigured is fine
	}
	switch p.Mode {
	case "compliance", "governance":
		// OK
	default:
		return fmt.Errorf("worm: invalid mode %q (want compliance|governance)", p.Mode)
	}
	if p.RetentionSeconds <= 0 {
		return fmt.Errorf("worm: retention_seconds must be > 0; got %d", p.RetentionSeconds)
	}
	if p.RetentionSeconds > MaxRetentionSeconds {
		return fmt.Errorf("worm: retention_seconds %d exceeds the maximum this can represent, "+
			"%d (~292 years) — a longer deadline overflows time.Duration and lands in the PAST, "+
			"which the storage backend treats as already expired and therefore as no lock at all. "+
			"Use a retention at or below 292y",
			p.RetentionSeconds, MaxRetentionSeconds)
	}
	return nil
}

// ParseWORMRetention parses an operator-friendly duration string
// into seconds. Accepts:
//
//	"<N>y"   N years (365-day years; calendar-quirk-immune)
//	"<N>d"   N days
//	"<N>h"   N hours
//	"<N>m"   N minutes
//
// Years are explicitly 365 days — calendar-aware "year" semantics
// are an opportunity for off-by-one bugs in compliance contexts.
// Operators wanting calendar-day-aware retention compute it
// themselves and pass days.
func ParseWORMRetention(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("retention is empty")
	}
	var num int64
	var unit byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= '0' && c <= '9' {
			d := int64(c - '0')
			// Bound the accumulator. Unbounded, a fat-fingered digit run
			// wraps silently: "9223372036854775807d" came out as -86400
			// seconds and "99999999999999999999d" as a positive 6.2e18,
			// which then passed Validate and overflowed RetainUntil.
			if num > (math.MaxInt64-d)/10 {
				return 0, fmt.Errorf("retention %q: value is too large (maximum is 292y)", s)
			}
			num = num*10 + d
			continue
		}
		// Non-digit: must be the trailing unit suffix.
		if i == 0 || i != len(s)-1 {
			return 0, fmt.Errorf("retention %q: expected digits followed by one of y|d|h|m", s)
		}
		unit = c
	}
	if unit == 0 {
		return 0, fmt.Errorf("retention %q: missing unit suffix (use y|d|h|m)", s)
	}
	if num <= 0 {
		return 0, fmt.Errorf("retention %q: must be > 0", s)
	}
	const (
		minute = 60
		hour   = 60 * minute
		day    = 24 * hour
		year   = 365 * day
	)
	var mult int64
	switch unit {
	case 'y', 'Y':
		mult = year
	case 'd', 'D':
		mult = day
	case 'h', 'H':
		mult = hour
	case 'm', 'M':
		mult = minute
	}
	if mult != 0 {
		// The unit multiply is the second place this overflows: "300000000000y"
		// wrapped to a large NEGATIVE second count.
		if num > MaxRetentionSeconds/mult {
			return 0, fmt.Errorf("retention %q: exceeds the maximum representable retention "+
				"of %d seconds (~292 years); beyond that the deadline overflows and lands in "+
				"the past, which is no lock at all", s, MaxRetentionSeconds)
		}
		return num * mult, nil
	}
	return 0, fmt.Errorf("retention %q: unknown unit %q (use y|d|h|m)", s, string(unit))
}

// MakeWORMPolicy is the operator-supplied form: parses retention,
// validates mode, and returns a complete WORMPolicy.
func MakeWORMPolicy(mode, retention string) (*WORMPolicy, error) {
	if mode == "" && retention == "" {
		return nil, nil // explicitly disabled
	}
	if mode == "" || retention == "" {
		return nil, fmt.Errorf("worm: --worm-mode and --worm-retention must be set together")
	}
	secs, err := ParseWORMRetention(retention)
	if err != nil {
		return nil, fmt.Errorf("worm: %w", err)
	}
	p := &WORMPolicy{
		Mode:             strings.ToLower(strings.TrimSpace(mode)),
		Retention:        retention,
		RetentionSeconds: secs,
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return p, nil
}
