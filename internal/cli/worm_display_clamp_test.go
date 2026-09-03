package cli

// What a compliance report says about WORM retention must match what is
// actually enforced.
//
// time.Duration is int64 nanoseconds and saturates near 292 years, so
// the unclamped `time.Duration(secs) * time.Second` in both display
// paths wrapped for any oversized stored value. A repository still
// carrying the 31536000000 seconds that "1000y" used to resolve to
// rendered as:
//
//	WORM:   compliance (-1488191h9m7.419103232s)
//
// A NEGATIVE retention, printed on the document an auditor reads. The
// enforcement side clamps (repo.WORMPolicy.RetainUntil), so the report
// was also contradicting the behaviour it describes.

import (
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

func TestDurationFromSeconds_ClampsInsteadOfWrapping(t *testing.T) {
	// Exactly what a pre-limit repository has in its metadata.
	const legacyThousandYears = 31536000000

	got := durationFromSeconds(legacyThousandYears)
	if strings.HasPrefix(got, "-") {
		t.Fatalf("durationFromSeconds(%d) = %q — a negative WORM retention on a "+
			"compliance report", legacyThousandYears, got)
	}
	// It must report the ENFORCED figure, which is the clamp.
	want := durationFromSeconds(repo.MaxRetentionSeconds)
	if got != want {
		t.Errorf("durationFromSeconds(%d) = %q, want the enforced maximum %q; the report "+
			"must not claim a retention the repository will not apply",
			legacyThousandYears, got, want)
	}
}

func TestDurationFromSeconds_OrdinaryValuesUnchanged(t *testing.T) {
	cases := map[int64]string{
		0:          "0s",
		-5:         "0s",
		604800:     "7d",     // 7 days
		3153600000: "36500d", // 100 years, comfortably representable
	}
	for secs, want := range cases {
		if got := durationFromSeconds(secs); got != want {
			t.Errorf("durationFromSeconds(%d) = %q, want %q", secs, got, want)
		}
	}
}

// The clamp boundary itself: one second past the maximum must still
// render as a positive duration, not wrap.
func TestDurationFromSeconds_BoundaryIsPositive(t *testing.T) {
	for _, secs := range []int64{
		repo.MaxRetentionSeconds - 1,
		repo.MaxRetentionSeconds,
		repo.MaxRetentionSeconds + 1,
		1 << 62,
	} {
		got := durationFromSeconds(secs)
		if strings.HasPrefix(got, "-") {
			t.Errorf("durationFromSeconds(%d) = %q (negative)", secs, got)
		}
	}
	// And sanity: the clamp really is ~292 years.
	d := time.Duration(repo.MaxRetentionSeconds) * time.Second
	if years := d.Hours() / 24 / 365; years < 291 || years > 293 {
		t.Errorf("MaxRetentionSeconds is %.1f years, expected ~292", years)
	}
}
