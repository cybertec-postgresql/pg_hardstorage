// identifier_property_test.go — generative tests for ValidIdentifier.
//
// PG identifiers shape: first byte letter or underscore, remaining
// letters / digits / underscores, length 1..63 bytes.  These rules
// must hold for every shape rapid generates.
package pg_test

import (
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
)

// Property 1: any string drawn from the PG identifier alphabet that
// starts with a letter/underscore and fits in 1..63 bytes must be
// accepted.
func TestProperty_ValidIdentifier_AcceptsLegal(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		first := rapid.RuneFrom([]rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ_")).
			Draw(t, "first")
		rest := rapid.StringMatching(`[a-zA-Z0-9_]{0,62}`).Draw(t, "rest")
		s := string(first) + rest
		if !pg.ValidIdentifier(s) {
			t.Errorf("ValidIdentifier rejected legal %q", s)
		}
	})
}

// Property 2: a string with any character outside [a-zA-Z0-9_] is
// rejected.
func TestProperty_ValidIdentifier_RejectsIllegalRune(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		legal := rapid.StringMatching(`[a-zA-Z_][a-zA-Z0-9_]{0,30}`).Draw(t, "legal")
		bad := rapid.RuneFrom([]rune("!@#$%^&*()-+= [].,;:'\"\\/<>?{}|`~")).Draw(t, "bad")
		pos := rapid.IntRange(0, len(legal)).Draw(t, "pos")
		mutated := legal[:pos] + string(bad) + legal[pos:]
		if pg.ValidIdentifier(mutated) {
			t.Errorf("ValidIdentifier accepted %q (legal %q + bad rune %q at %d)",
				mutated, legal, string(bad), pos)
		}
	})
}

// Property 3: any string starting with a digit is rejected, no
// matter what follows.  PG requires letter or underscore first.
func TestProperty_ValidIdentifier_RejectsLeadingDigit(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		first := rapid.RuneFrom([]rune("0123456789")).Draw(t, "digit")
		rest := rapid.StringMatching(`[a-zA-Z0-9_]{0,30}`).Draw(t, "rest")
		s := string(first) + rest
		if pg.ValidIdentifier(s) {
			t.Errorf("ValidIdentifier accepted leading-digit %q", s)
		}
	})
}

// Property 4: length 0 and length 64+ are rejected (NAMEDATALEN-1).
func TestProperty_ValidIdentifier_RejectsOutOfRangeLengths(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate a name of length 64..200 made of legal chars.
		// The shape is otherwise valid; only the length should fail.
		n := rapid.IntRange(64, 200).Draw(t, "len")
		s := "a" + strings.Repeat("b", n-1)
		if pg.ValidIdentifier(s) {
			t.Errorf("ValidIdentifier accepted %d-char identifier (>63 cap)", len(s))
		}
	})
	if pg.ValidIdentifier("") {
		t.Error("ValidIdentifier accepted empty string")
	}
}

// Acceptance smoke: hand-rolled illegal set that should ALL fail.
func TestValidIdentifier_HandRolledInvalids(t *testing.T) {
	for _, bad := range []string{
		"",
		"1abc",                  // leading digit
		"a-b",                   // hyphen
		"a.b",                   // dot
		"a b",                   // space
		"a/b",                   // slash
		"a;b",                   // punctuation
		strings.Repeat("a", 64), // too long
	} {
		if pg.ValidIdentifier(bad) {
			t.Errorf("ValidIdentifier accepted %q", bad)
		}
	}
}
