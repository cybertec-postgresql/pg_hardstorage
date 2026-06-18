// parse_dst_test.go — daylight-saving / timezone edge cases.
//
// PITR targets are frequently typed at 3am during an incident, often
// hours after a DST transition. The package docstring promises "no
// surprises at 3am"; these tests pin that promise across the two
// hazardous transitions. Europe/Berlin 2026:
//
//	spring-forward: Sun 2026-03-29, 02:00 CET (+01) → 03:00 CEST (+02)
//	                — wall times in [02:00,03:00) do not exist.
//	fall-back:      Sun 2026-10-25, 03:00 CEST (+02) → 02:00 CET (+01)
//	                — wall times in [02:00,03:00) occur twice.
package naturaltime_test

import (
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore/naturaltime"
)

// Relative expressions ("N hours/days ago") are absolute-duration
// arithmetic and MUST land on the exact instant regardless of any DST
// transition crossed. This is the property a PITR operator relies on
// most: "5 hours ago" means 5 real hours, not "5 hours of wall clock
// that might be 4 or 6 because the clocks changed."
func TestParse_RelativeAcrossDST_IsInstantAccurate(t *testing.T) {
	berlin := mustLoc("Europe/Berlin")

	cases := []struct {
		name string
		ref  time.Time
		in   string
		want time.Time
	}{
		{
			// ref = just after spring-forward (03:30 CEST = 01:30 UTC).
			// 3 real hours earlier crosses the skipped hour.
			name: "spring-forward 3h ago",
			ref:  time.Date(2026, 3, 29, 3, 30, 0, 0, berlin),
			in:   "3 hours ago",
			want: time.Date(2026, 3, 28, 22, 30, 0, 0, time.UTC),
		},
		{
			// ref = just after fall-back (04:00 CET = 03:00 UTC).
			// 3 real hours earlier crosses the repeated hour.
			name: "fall-back 3h ago",
			ref:  time.Date(2026, 10, 25, 4, 0, 0, 0, berlin),
			in:   "3 hours ago",
			want: time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC),
		},
		{
			// 90 minutes spanning the spring-forward gap: 03:30 CEST
			// back 90 real minutes = 00:00 UTC (01:00 CET).
			name: "spring-forward 90m ago",
			ref:  time.Date(2026, 3, 29, 3, 30, 0, 0, berlin),
			in:   "90 minutes ago",
			want: time.Date(2026, 3, 29, 0, 0, 0, 0, time.UTC),
		},
		{
			// "1 day ago" is calendar-based (AddDate), but midnight is
			// not a transition point, so the instant is still exact.
			name: "1 day ago over spring-forward weekend",
			ref:  time.Date(2026, 3, 30, 12, 0, 0, 0, berlin),
			in:   "1 day ago",
			want: time.Date(2026, 3, 29, 12, 0, 0, 0, berlin).UTC(),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := naturaltime.Parse(c.in, c.ref)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Equal(c.want) {
				t.Errorf("%q from %s:\n  got  %s\n  want %s",
					c.in, c.ref.Format(time.RFC3339), got.Format(time.RFC3339), c.want.Format(time.RFC3339))
			}
		})
	}
}

// "yesterday HH:MM" must interpret the wall clock in YESTERDAY's
// offset, not today's. The day after spring-forward, "yesterday noon"
// is noon CEST (+02) = 10:00 UTC. A naive implementation that applied
// today's offset, or that computed the date in UTC, would drift by an
// hour. This is the most refactor-fragile branch (parseRelativeDay's
// date is taken in loc, and the clock re-applied in loc).
func TestParse_Yesterday_UsesTargetDateOffset(t *testing.T) {
	berlin := mustLoc("Europe/Berlin")
	ref := time.Date(2026, 3, 30, 12, 0, 0, 0, berlin) // Monday after the change

	got, err := naturaltime.Parse("yesterday 12:00", ref)
	if err != nil {
		t.Fatal(err)
	}
	// Yesterday = 2026-03-29 (already past the 02:00 transition), so
	// noon is CEST (+02) → 10:00 UTC.
	want := time.Date(2026, 3, 29, 12, 0, 0, 0, berlin).UTC()
	if !got.Equal(want) {
		t.Errorf("yesterday 12:00:\n  got  %s\n  want %s (noon CEST)",
			got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
	if u := got.UTC(); u.Hour() != 10 {
		t.Errorf("expected 10:00 UTC (noon CEST); got %s", u.Format("15:04:05Z07:00"))
	}
}

// A wall-clock time inside the spring-forward GAP does not exist; Go
// normalizes it deterministically (to the pre-transition offset). We
// don't over-pin Go's choice, but we DO pin two things an operator
// needs: (1) it never errors, and (2) it's stable — the same input
// resolves to the same instant every time, and to the same instant in
// both the seed-resolution and recovery-arming call sites (which both
// reuse one parse). Determinism is what makes the PITR seed/target
// agree.
func TestParse_SpringForwardGap_Deterministic(t *testing.T) {
	berlin := mustLoc("Europe/Berlin")
	ref := time.Date(2026, 3, 29, 3, 30, 0, 0, berlin)

	first, err := naturaltime.Parse("today 02:30", ref)
	if err != nil {
		t.Fatalf("gap time must not error: %v", err)
	}
	for i := 0; i < 5; i++ {
		again, err := naturaltime.Parse("today 02:30", ref)
		if err != nil || !again.Equal(first) {
			t.Fatalf("gap parse not deterministic: %v vs %v (err %v)", again, first, err)
		}
	}
	// The normalized instant must be within the hour around the gap
	// (between 00:00 and 02:00 UTC on the transition day), never wildly
	// off.
	lo := time.Date(2026, 3, 29, 0, 0, 0, 0, time.UTC)
	hi := time.Date(2026, 3, 29, 2, 0, 0, 0, time.UTC)
	if first.Before(lo) || first.After(hi) {
		t.Errorf("gap instant %s outside the plausible [%s,%s] window",
			first.UTC().Format(time.RFC3339), lo.Format(time.RFC3339), hi.Format(time.RFC3339))
	}
}

// A wall-clock time inside the fall-back OVERLAP occurs twice; Go
// picks one deterministically. Same contract as the gap: no error,
// stable, plausible window.
func TestParse_FallBackOverlap_Deterministic(t *testing.T) {
	berlin := mustLoc("Europe/Berlin")
	ref := time.Date(2026, 10, 25, 4, 0, 0, 0, berlin)

	first, err := naturaltime.Parse("today 02:30", ref)
	if err != nil {
		t.Fatalf("overlap time must not error: %v", err)
	}
	again, _ := naturaltime.Parse("today 02:30", ref)
	if !again.Equal(first) {
		t.Errorf("overlap parse not deterministic: %v vs %v", again, first)
	}
	// 02:30 in the overlap is either 00:30 UTC (CEST) or 01:30 UTC (CET).
	a := time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC)
	b := time.Date(2026, 10, 25, 1, 30, 0, 0, time.UTC)
	if !first.Equal(a) && !first.Equal(b) {
		t.Errorf("overlap instant %s is neither CEST (%s) nor CET (%s) occurrence",
			first.UTC().Format(time.RFC3339), a.Format(time.RFC3339), b.Format(time.RFC3339))
	}
}

// Half-hour and quarter-hour offset zones (no DST) must apply the full
// offset to "today/yesterday" wall times. Kathmandu is +05:45.
func TestParse_FractionalOffsetZone_TodayYesterday(t *testing.T) {
	ktm := mustLoc("Asia/Kathmandu")
	ref := time.Date(2026, 4, 28, 14, 0, 0, 0, ktm)

	got, err := naturaltime.Parse("today 09:00", ref)
	if err != nil {
		t.Fatal(err)
	}
	// 09:00 +05:45 = 03:15 UTC.
	want := time.Date(2026, 4, 28, 3, 15, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("today 09:00 in +05:45:\n  got  %s\n  want %s",
			got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// Whatever the operator's local zone, Parse always returns a UTC
// time.Time, so the downstream recovery_target_time render (which
// does .UTC().Format) is timezone- and DST-agnostic by construction.
func TestParse_AlwaysReturnsUTC(t *testing.T) {
	for _, zone := range []string{"Europe/Berlin", "Asia/Kathmandu", "America/New_York", "UTC"} {
		loc := mustLoc(zone)
		ref := time.Date(2026, 3, 29, 3, 30, 0, 0, loc)
		for _, in := range []string{"now", "2 hours ago", "today 09:00", "yesterday 21:00"} {
			got, err := naturaltime.Parse(in, ref)
			if err != nil {
				t.Errorf("%s/%q: %v", zone, in, err)
				continue
			}
			if got.Location() != time.UTC {
				t.Errorf("%s/%q: result location = %v; want UTC", zone, in, got.Location())
			}
		}
	}
}
