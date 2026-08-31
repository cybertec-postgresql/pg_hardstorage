package durationx_test

// The parser itself. Its behaviour is pinned here rather than in
// internal/cli so the config path — which is what issue #54 was about —
// has coverage that does not depend on a flag being registered.

import (
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/durationx"
)

func TestParse_DayAndWeekUnits(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"3d", 3 * 24 * time.Hour},
		{"7w", 7 * 7 * 24 * time.Hour},
		{"30d", 30 * 24 * time.Hour},
		{"1w", 7 * 24 * time.Hour},
		{"1w2d", 9 * 24 * time.Hour},
		{"1d12h", 36 * time.Hour},
		{"0d", 0},
		{"0.5d", 12 * time.Hour},
		// Plain stdlib shapes must be untouched.
		{"6h", 6 * time.Hour},
		{"30m", 30 * time.Minute},
		{"5s", 5 * time.Second},
		{"1h30m", 90 * time.Minute},
		{"-3d", -3 * 24 * time.Hour},
	}
	for _, c := range cases {
		got, err := durationx.Parse(c.in)
		if err != nil {
			t.Errorf("Parse(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("Parse(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// The exact spellings from issue #54's reproduction.
func TestParse_Issue54Spellings(t *testing.T) {
	for _, in := range []string{"7w", "3d"} {
		if _, err := durationx.Parse(in); err != nil {
			t.Errorf("Parse(%q) failed — this is the value the reporter had in "+
				"pg_hardstorage.yaml: %v", in, err)
		}
	}
}

func TestParse_RejectsGarbage(t *testing.T) {
	for _, in := range []string{"", " ", "d", "w", "abc", "3x", "+", "-", "++1h", "+-1h", "3d4x"} {
		if d, err := durationx.Parse(in); err == nil {
			t.Errorf("Parse(%q) = %v, want an error", in, d)
		}
	}
}

// Days are wall-clock 24h, not calendar days. Stated so a future
// change to calendar arithmetic — which would make retention depend on
// WHEN it ran — is a deliberate decision.
func TestParse_DaysAreExactlyTwentyFourHours(t *testing.T) {
	got, err := durationx.Parse("1d")
	if err != nil {
		t.Fatal(err)
	}
	if got != 24*time.Hour {
		t.Errorf("1d = %v, want 24h", got)
	}
	w, err := durationx.Parse("1w")
	if err != nil {
		t.Fatal(err)
	}
	if w != 7*24*time.Hour {
		t.Errorf("1w = %v, want 168h", w)
	}
}

// Anything time.ParseDuration accepts must parse identically — the
// parser widens the language, it does not change it.
func TestParse_AgreesWithStdlibWhereStdlibAccepts(t *testing.T) {
	for _, in := range []string{"0s", "1ns", "1us", "1ms", "1s", "1m", "1h",
		"1h2m3s", "-1h", "1.5h", "100h"} {
		want, serr := time.ParseDuration(in)
		if serr != nil {
			t.Fatalf("fixture %q is not a stdlib duration: %v", in, serr)
		}
		got, err := durationx.Parse(in)
		if err != nil {
			t.Errorf("Parse(%q) rejected a valid stdlib duration: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("Parse(%q) = %v, stdlib says %v", in, got, want)
		}
	}
}
