// worm.go — WORMPolicy: per-object retention deadline propagated to storage Object-Lock at PUT.
package repo

import (
	"fmt"
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
	return now.Add(time.Duration(p.RetentionSeconds) * time.Second).UTC()
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
			num = num*10 + int64(c-'0')
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
	switch unit {
	case 'y', 'Y':
		return num * year, nil
	case 'd', 'D':
		return num * day, nil
	case 'h', 'H':
		return num * hour, nil
	case 'm', 'M':
		return num * minute, nil
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
