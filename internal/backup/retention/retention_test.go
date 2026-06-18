package retention_test

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/retention"
)

// mk builds a manifest stamped at t with a deterministic ID. id is
// also returned so tests can identify expected entries by name.
func mk(t time.Time) *backup.Manifest {
	id := "db1." + t.UTC().Format("20060102T150405Z")
	return &backup.Manifest{
		BackupID:   id,
		Deployment: "db1",
		Type:       backup.BackupTypeFull,
		StoppedAt:  t.UTC(),
	}
}

// idsOf extracts BackupIDs in order. Sorted for stable comparisons
// when policy ordering is not what's under test.
func idsOf(in []*backup.Manifest) []string {
	out := make([]string, len(in))
	for i, m := range in {
		out[i] = m.BackupID
	}
	return out
}

// Pinned reference instant (not-on-a-day-boundary by design).
var refUTC = time.Date(2026, 4, 28, 14, 21, 8, 0, time.UTC)

func TestGFS_PicksOnePerDayBucket(t *testing.T) {
	// Three backups today, two yesterday, one day-before. KeepDaily=2.
	in := []*backup.Manifest{
		mk(refUTC),
		mk(refUTC.Add(-1 * time.Hour)),
		mk(refUTC.Add(-3 * time.Hour)),
		mk(refUTC.Add(-26 * time.Hour)),
		mk(refUTC.Add(-30 * time.Hour)),
		mk(refUTC.Add(-50 * time.Hour)),
	}
	d := retention.GFSPolicy{KeepDaily: 2}.Apply(refUTC, in)

	if d.KeptCount() != 2 {
		t.Errorf("kept = %d, want 2", d.KeptCount())
	}
	// Newest in today's bucket and newest in yesterday's bucket.
	want := []string{
		idsOf([]*backup.Manifest{mk(refUTC)})[0],
		idsOf([]*backup.Manifest{mk(refUTC.Add(-26 * time.Hour))})[0],
	}
	got := idsOf(d.Keep)
	sort.Strings(got)
	sort.Strings(want)
	if !equalStr(got, want) {
		t.Errorf("kept = %v, want %v", got, want)
	}
}

func TestGFS_NewestAlwaysKept(t *testing.T) {
	// KeepDaily=0 should still keep the newest backup.
	in := []*backup.Manifest{mk(refUTC), mk(refUTC.Add(-1 * time.Hour))}
	d := retention.GFSPolicy{}.Apply(refUTC, in)
	if d.KeptCount() != 1 {
		t.Errorf("kept = %d, want 1 (the newest, by safety-net rule)", d.KeptCount())
	}
	if !contains(d.Reasons[d.Keep[0].BackupID], "newest") {
		t.Errorf("newest backup missing 'newest' reason; reasons = %v", d.Reasons)
	}
}

func TestGFS_AccumulatesReasonsAcrossBuckets(t *testing.T) {
	// One backup per day for 8 days + one from a month ago. With
	// KeepDaily=7, KeepMonthly=2, the monthly should be the month-ago
	// backup; the most-recent backup picks up daily-1 + weekly-1 +
	// monthly-1 reasons (all rolling up to it).
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	in := []*backup.Manifest{}
	for i := 0; i < 8; i++ {
		in = append(in, mk(now.Add(time.Duration(-i)*24*time.Hour)))
	}
	monthAgo := time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)
	monthly := mk(monthAgo)
	in = append(in, monthly)

	d := retention.GFSPolicy{KeepDaily: 7, KeepWeekly: 2, KeepMonthly: 2}.Apply(now, in)

	// The newest manifest should claim daily-1, weekly-1, monthly-1.
	newestID := mk(now).BackupID
	got := d.Reasons[newestID]
	for _, want := range []string{"daily-1", "weekly-1", "monthly-1"} {
		if !contains(got, want) {
			t.Errorf("newest backup missing reason %q; got %v", want, got)
		}
	}
	// The month-ago backup should claim monthly-2.
	got = d.Reasons[monthly.BackupID]
	if !contains(got, "monthly-2") {
		t.Errorf("month-ago backup should be monthly-2; got %v", got)
	}
}

func TestGFS_KeepCountsCappedByAvailable(t *testing.T) {
	// Only 2 backups but KeepDaily=10 — keep both, no error.
	in := []*backup.Manifest{
		mk(refUTC),
		mk(refUTC.Add(-25 * time.Hour)),
	}
	d := retention.GFSPolicy{KeepDaily: 10}.Apply(refUTC, in)
	if d.KeptCount() != 2 || d.DeletedCount() != 0 {
		t.Errorf("kept=%d deleted=%d; want 2/0", d.KeptCount(), d.DeletedCount())
	}
}

func TestGFS_ZeroPolicyKeepsOnlyNewest(t *testing.T) {
	in := []*backup.Manifest{mk(refUTC), mk(refUTC.Add(-time.Hour)), mk(refUTC.Add(-25 * time.Hour))}
	d := retention.GFSPolicy{}.Apply(refUTC, in)
	if d.KeptCount() != 1 {
		t.Errorf("kept = %d, want 1", d.KeptCount())
	}
	if d.Keep[0].BackupID != mk(refUTC).BackupID {
		t.Errorf("kept = %v, want newest", d.Keep[0].BackupID)
	}
}

func TestGFS_EmptyInput(t *testing.T) {
	d := retention.GFSPolicy{KeepDaily: 5}.Apply(refUTC, nil)
	if d.KeptCount() != 0 || d.DeletedCount() != 0 {
		t.Errorf("empty: kept=%d deleted=%d", d.KeptCount(), d.DeletedCount())
	}
}

func TestSimple_KeepsByAge(t *testing.T) {
	in := []*backup.Manifest{
		mk(refUTC),
		mk(refUTC.Add(-2 * time.Hour)),
		mk(refUTC.Add(-72 * time.Hour)),
		mk(refUTC.Add(-30 * 24 * time.Hour)),
	}
	d := retention.SimplePolicy{KeepFor: 24 * time.Hour}.Apply(refUTC, in)
	// Keep newest two (within 24h), delete older two.
	if d.KeptCount() != 2 {
		t.Errorf("kept = %d, want 2 (within 24h)", d.KeptCount())
	}
	if d.DeletedCount() != 2 {
		t.Errorf("deleted = %d, want 2", d.DeletedCount())
	}
}

func TestSimple_KeepsAtLeastNewest(t *testing.T) {
	// Only the newest exists, but it's outside the window. Newest
	// safety-net rule still keeps it.
	in := []*backup.Manifest{mk(refUTC.Add(-100 * 24 * time.Hour))}
	d := retention.SimplePolicy{KeepFor: 24 * time.Hour}.Apply(refUTC, in)
	if d.KeptCount() != 1 {
		t.Errorf("kept = %d, want 1 (safety-net)", d.KeptCount())
	}
}

func TestCount_KeepsLastNFulls(t *testing.T) {
	in := []*backup.Manifest{
		mk(refUTC),
		mk(refUTC.Add(-time.Hour)),
		mk(refUTC.Add(-2 * time.Hour)),
		mk(refUTC.Add(-3 * time.Hour)),
		mk(refUTC.Add(-4 * time.Hour)),
	}
	d := retention.CountPolicy{KeepFulls: 3}.Apply(refUTC, in)
	if d.KeptCount() != 3 {
		t.Errorf("kept = %d, want 3", d.KeptCount())
	}
	got := idsOf(d.Keep)
	want := idsOf(in[:3]) // newest 3
	if !equalStr(got, want) {
		t.Errorf("kept = %v, want newest 3 = %v", got, want)
	}
}

func TestCount_ZeroKeepsOnlyNewest(t *testing.T) {
	in := []*backup.Manifest{mk(refUTC), mk(refUTC.Add(-time.Hour))}
	d := retention.CountPolicy{KeepFulls: 0}.Apply(refUTC, in)
	if d.KeptCount() != 1 {
		t.Errorf("kept = %d, want 1 (safety-net)", d.KeptCount())
	}
}

func TestPolicyName(t *testing.T) {
	cases := []struct {
		p    retention.Policy
		want string
	}{
		{retention.GFSPolicy{}, "gfs"},
		{retention.SimplePolicy{}, "simple"},
		{retention.CountPolicy{}, "count"},
	}
	for _, c := range cases {
		if got := c.p.Name(); got != c.want {
			t.Errorf("%T.Name() = %q, want %q", c.p, got, c.want)
		}
	}
}

func TestGFS_ISOWeekBucketing(t *testing.T) {
	// Backups straddling a week boundary should land in different
	// weekly buckets. Pick a Monday and the preceding Sunday.
	monday := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC) // ISO week 18 of 2026
	sunday := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC) // ISO week 17 of 2026
	in := []*backup.Manifest{mk(monday), mk(sunday)}

	d := retention.GFSPolicy{KeepWeekly: 2}.Apply(time.Date(2026, 4, 28, 0, 0, 0, 0, time.UTC), in)
	if d.KeptCount() != 2 {
		t.Errorf("kept = %d, want 2 (different ISO weeks)", d.KeptCount())
	}
}

func TestDecision_ReasonsAreOrderStable(t *testing.T) {
	// Multi-reason backup should accumulate reasons in the
	// daily/weekly/monthly/yearly order they're applied. This is
	// what the operator sees in the rotate output, so order matters.
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	newest := mk(now)
	in := []*backup.Manifest{newest}
	d := retention.GFSPolicy{KeepDaily: 1, KeepWeekly: 1, KeepMonthly: 1, KeepYearly: 1}.Apply(now, in)

	got := strings.Join(d.Reasons[newest.BackupID], " ")
	want := "daily-1 weekly-1 monthly-1 yearly-1"
	if got != want {
		t.Errorf("reasons = %q, want %q", got, want)
	}
}

func TestPartitionInvariant(t *testing.T) {
	// For any input, |Keep| + |Delete| == |input|, and Keep ∩ Delete is empty.
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	in := []*backup.Manifest{}
	for i := 0; i < 30; i++ {
		in = append(in, mk(now.Add(time.Duration(-i)*24*time.Hour)))
	}
	d := retention.GFSPolicy{KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 3}.Apply(now, in)
	if got := d.KeptCount() + d.DeletedCount(); got != len(in) {
		t.Errorf("partition broken: kept+deleted = %d, input = %d", got, len(in))
	}
	keptSet := map[string]bool{}
	for _, m := range d.Keep {
		keptSet[m.BackupID] = true
	}
	for _, m := range d.Delete {
		if keptSet[m.BackupID] {
			t.Errorf("manifest %s in both Keep and Delete", m.BackupID)
		}
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func equalStr(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// fmtForce is unused — kept here intentionally so future reason-
// formatting changes don't strand this import.
var _ = fmt.Sprintf
