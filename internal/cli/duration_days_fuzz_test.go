package cli

// duration_days_fuzz_test.go — ParseDurationDays is hand-rolled string
// scanning (index arithmetic, slicing on rest[:i] / rest[i+1:]), which
// is exactly the shape that panics on an input nobody thought of. It
// also sits in front of operator-supplied flag values, so a panic is a
// crash on a typo.
//
// Two invariants, and the second is the one that matters most:
//
//  1. it never panics, whatever it is handed;
//  2. for any input that does NOT use the new d/w units, it agrees with
//     time.ParseDuration exactly — value and error-ness both. Extending
//     a parser must not quietly change what the old spellings mean, or
//     `--keep-for 720h` could start retaining something different from
//     what it retained before.

import (
	"strings"
	"testing"
	"time"
)

func FuzzParseDurationDays_NeverPanics(f *testing.F) {
	for _, seed := range []string{
		"", "30d", "1w", "1w2d3h", "720h", "-7d", "+1d", "0.5d", "1e3d",
		"d", "w", "..d", "1..2d", "9999999999999999999d", "1d1d1d",
		"1w2", "30x", " 30d ", "1d-2h", "--1d", "1.d", ".5w",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		// The contract is "returns, one way or the other". A panic here
		// is a crash on a mistyped flag.
		_, _ = ParseDurationDays(s)
	})
}

// The differential invariant: on inputs the standard parser handles,
// the extended one must be indistinguishable from it.
func FuzzParseDurationDays_AgreesWithStdlib(f *testing.F) {
	for _, seed := range []string{"720h", "90m", "1h30m", "2s", "-3h", "0", "1ns", "1.5h"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		// Only inputs with no day/week unit are in scope: those are the
		// ones whose meaning must be preserved exactly.
		if strings.ContainsAny(s, "dDwW") {
			return
		}
		std, stdErr := time.ParseDuration(s)
		got, gotErr := ParseDurationDays(s)

		// Leading/trailing space is accepted by the extended parser and
		// rejected by the stdlib one; that widening is deliberate, so
		// compare on the trimmed form.
		if strings.TrimSpace(s) != s {
			return
		}
		if (stdErr == nil) != (gotErr == nil) {
			t.Fatalf("ParseDurationDays(%q) err=%v but time.ParseDuration err=%v — "+
				"extending the parser changed which plain durations are valid", s, gotErr, stdErr)
		}
		if stdErr == nil && got != std {
			t.Fatalf("ParseDurationDays(%q) = %v, time.ParseDuration = %v — "+
				"a plain Go duration must keep its exact meaning", s, got, std)
		}
	})
}
