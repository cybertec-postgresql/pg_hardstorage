// gfs.go — GFSPolicy: grandfather-father-son daily/weekly/monthly/yearly bucketing.
package retention

import (
	"fmt"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
)

// GFSPolicy implements grandfather-father-son retention.
//
// For each (daily, weekly, monthly, yearly) bucket, we keep up to N
// manifests, choosing the most-recent manifest within each calendar
// bucket. A manifest may satisfy multiple buckets simultaneously —
// e.g. a backup taken on the first day of a new month is the
// daily-1, weekly-1 (assuming Monday-aligned ISO weeks fall right),
// AND the monthly-1 representative for that calendar month. Reasons
// accumulate.
//
// The "calendar bucket" key is computed in UTC so daylight-saving
// transitions don't shift bucket boundaries. Operators thinking in
// local time need to make their peace with this — alternative:
// configurable RetentionTimezone, deferred to a future revision.
type GFSPolicy struct {
	KeepDaily   int
	KeepWeekly  int
	KeepMonthly int
	KeepYearly  int
}

// Name implements Policy.
func (GFSPolicy) Name() string { return "gfs" }

// Apply implements Policy. The algorithm walks manifests sorted
// newest-first; for each policy bucket (day / ISO week / calendar
// month / calendar year), the first manifest seen in a previously-
// unseen bucket key is selected. Iteration stops for a bucket once
// its KeepX limit is reached.
func (p GFSPolicy) Apply(now time.Time, in []*backup.Manifest) Decision {
	d := Decision{PolicyName: p.Name()}
	sorted := sortByStoppedAtDesc(in)

	pickPerBucket(&d, sorted, p.KeepDaily, "daily", dailyKey)
	pickPerBucket(&d, sorted, p.KeepWeekly, "weekly", isoWeekKey)
	pickPerBucket(&d, sorted, p.KeepMonthly, "monthly", monthlyKey)
	pickPerBucket(&d, sorted, p.KeepYearly, "yearly", yearlyKey)

	finalize(&d, sorted)
	return d
}

// keyFn extracts a bucket key from a manifest's StoppedAt. Always UTC.
type keyFn func(time.Time) string

func dailyKey(t time.Time) string {
	t = t.UTC()
	return fmt.Sprintf("%04d-%02d-%02d", t.Year(), t.Month(), t.Day())
}

func isoWeekKey(t time.Time) string {
	t = t.UTC()
	year, week := t.ISOWeek()
	return fmt.Sprintf("%04d-W%02d", year, week)
}

func monthlyKey(t time.Time) string {
	t = t.UTC()
	return fmt.Sprintf("%04d-%02d", t.Year(), t.Month())
}

func yearlyKey(t time.Time) string {
	t = t.UTC()
	return fmt.Sprintf("%04d", t.Year())
}

// pickPerBucket walks sorted (newest-first) and selects the first
// manifest in each previously-unseen bucket key, up to keep
// selections. Records a "<label>-<n>" reason on each selected
// manifest so the operator can see "daily-1 daily-2 ...".
//
// keep <= 0 disables the bucket entirely (no selections recorded).
func pickPerBucket(d *Decision, sorted []*backup.Manifest, keep int, label string, key keyFn) {
	if keep <= 0 {
		return
	}
	seen := map[string]struct{}{}
	picked := 0
	for _, m := range sorted {
		k := key(m.StoppedAt)
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		picked++
		d.addReason(m.BackupID, fmt.Sprintf("%s-%d", label, picked))
		if picked >= keep {
			return
		}
	}
}
