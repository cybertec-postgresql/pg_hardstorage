package repo_test

// A WORM retention that cannot be represented must be refused, never
// silently turned into no protection.
//
// RetainUntil computes now.Add(RetentionSeconds * time.Second), and
// time.Duration is int64 NANOSECONDS, so it saturates around 292 years.
// Past that the multiply wraps and the deadline lands in the PAST:
//
//	292y -> 2318-06-25   (correct)
//	293y -> 1734-12-04   (in the past)
//	1000y -> 1856-11-25  (in the past)
//
// A backend handed an already-expired ObjectLockRetainUntilDate either
// rejects the PUT or accepts it as expired. Either way an operator who
// asked to keep data effectively forever — "1000y" is an ordinary way to
// spell that in an archival policy — got no WORM lock at all, on exactly
// the repositories where that matters most, and nothing said so.
//
// Two more wrap points fed the same outcome: the digit accumulator in
// ParseWORMRetention ("9223372036854775807d" came out as -86400 seconds)
// and the unit multiply ("300000000000y" wrapped large and negative).
// Validate's `> 0` check caught the negative ones by luck; the positive
// overflow "99999999999999999999d" sailed through it.

import (
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

func TestParseWORMRetention_RefusesUnrepresentableDurations(t *testing.T) {
	cases := []struct {
		in  string
		why string
	}{
		{"1000y", "a plausible archival \"keep forever\", 1000 years"},
		{"293y", "one year past the representable maximum"},
		{"99999999999999999999d", "digit run that overflows the accumulator to a POSITIVE value"},
		{"9223372036854775807d", "int64 max as a digit run"},
		{"300000000000y", "overflows on the unit multiply"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := repo.ParseWORMRetention(c.in)
			if err == nil {
				t.Fatalf("%s: accepted, yielding %d seconds.\n\n"+
					"RetainUntil would then overflow and produce a deadline in the past, "+
					"which the backend treats as already expired — the operator asked for "+
					"maximum protection and received none.", c.why, got)
			}
			if !strings.Contains(err.Error(), "292") && !strings.Contains(err.Error(), "too large") {
				t.Errorf("error does not tell the operator the limit: %v", err)
			}
		})
	}
}

func TestParseWORMRetention_AcceptsRepresentableDurations(t *testing.T) {
	for _, in := range []string{"1m", "24h", "7d", "30d", "7y", "100y", "292y"} {
		secs, err := repo.ParseWORMRetention(in)
		if err != nil {
			t.Errorf("%s rejected: %v", in, err)
			continue
		}
		if secs <= 0 || secs > repo.MaxRetentionSeconds {
			t.Errorf("%s -> %d seconds, outside (0, %d]", in, secs, repo.MaxRetentionSeconds)
		}
	}
}

// The property that actually matters: a configured retention must never
// produce a deadline at or before the moment it is computed.
func TestRetainUntil_IsNeverInThePast(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	for _, in := range []string{"1m", "24h", "7d", "7y", "100y", "292y"} {
		p, err := repo.MakeWORMPolicy("compliance", in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		ru := p.RetainUntil(now)
		if !ru.After(now) {
			t.Errorf("retention %q produced RetainUntil=%s, which is not after now=%s.\n\n"+
				"An expired deadline is indistinguishable from no lock.",
				in, ru.Format(time.RFC3339), now.Format(time.RFC3339))
		}
	}
}

// A repository initialised before the limit was enforced still carries an
// oversized RetentionSeconds in its metadata, and RetainUntil reads that
// on every PUT. It must clamp to the maximum, not wrap into the past:
// of the two ways to be wrong, only one keeps the bytes.
func TestRetainUntil_ClampsMetadataFromBeforeTheLimit(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	// Constructed directly, as it would be when decoded from HSREPO.
	p := &repo.WORMPolicy{
		Mode:             "compliance",
		Retention:        "1000y",
		RetentionSeconds: 31536000000, // what 1000y used to resolve to
	}
	ru := p.RetainUntil(now)
	if !ru.After(now) {
		t.Fatalf("legacy metadata produced RetainUntil=%s, before now=%s — the repository "+
			"silently loses WORM protection on every object written after the upgrade",
			ru.Format(time.RFC3339), now.Format(time.RFC3339))
	}
	want := now.Add(time.Duration(repo.MaxRetentionSeconds) * time.Second).UTC()
	if !ru.Equal(want) {
		t.Errorf("RetainUntil = %s, want the clamped maximum %s",
			ru.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

func TestWORMPolicy_ValidateRejectsOversizedRetention(t *testing.T) {
	p := &repo.WORMPolicy{
		Mode: "compliance", Retention: "1000y",
		RetentionSeconds: repo.MaxRetentionSeconds + 1,
	}
	err := p.Validate()
	if err == nil {
		t.Fatal("Validate accepted a retention past the representable maximum")
	}
	if !strings.Contains(err.Error(), "292") {
		t.Errorf("error does not name the limit: %v", err)
	}
}
