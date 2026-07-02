// simple.go — SimplePolicy: age-based "keep within duration" retention.
package retention

import (
	"fmt"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
)

// SimplePolicy keeps every manifest younger than KeepFor and deletes
// the rest. A backup whose StoppedAt is exactly KeepFor old is kept
// (the comparison is strict-less-than for the cut-off).
//
// The newest backup is always kept regardless (the safety-net rule
// shared with GFSPolicy).
type SimplePolicy struct {
	KeepFor time.Duration
}

// Name implements Policy.
func (SimplePolicy) Name() string { return "simple" }

// Apply implements Policy.
func (p SimplePolicy) Apply(now time.Time, in []*backup.Manifest) Decision {
	d := Decision{PolicyName: p.Name()}
	sorted := sortByStoppedAtDesc(in)

	cutoff := now.Add(-p.KeepFor)
	for _, m := range sorted {
		// Keep every backup whose StoppedAt is at or newer than the
		// cutoff. The documented contract (see the type comment) is
		// that a backup EXACTLY KeepFor old is kept, so the boundary
		// comparison is !Before (>= cutoff), not After (> cutoff) —
		// the latter deleted the exact-boundary backup.
		if !m.StoppedAt.Before(cutoff) {
			d.addReason(m.BackupID, fmt.Sprintf("within-%s", p.KeepFor))
		}
	}

	finalize(&d, sorted)
	return d
}
