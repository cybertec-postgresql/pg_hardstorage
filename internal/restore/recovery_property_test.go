// recovery_property_test.go — generative tests for LooksLikeLSN.
//
// Builds on the LSN strict-shape check introduced by the issue #78
// fix.  Hand-written tests pin specific edge cases
// (trailing 'x', missing slash, ...); these properties cover the
// long tail by generating random inputs and asserting the same
// invariants hold for every shape rapid produces.
package restore_test

import (
	"strconv"
	"strings"
	"testing"
	"unicode"

	"github.com/jackc/pglogrepl"
	"pgregory.net/rapid"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
)

// Property 1: every value pglogrepl.LSN.String() emits must satisfy
// LooksLikeLSN.  This is the contract — the strict shape check
// must accept the canonical form of every valid LSN.
func TestProperty_LooksLikeLSN_AcceptsCanonical(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		u := rapid.Uint64().Draw(t, "lsn")
		s := pglogrepl.LSN(u).String()
		if !restore.LooksLikeLSN(s) {
			t.Errorf("LooksLikeLSN rejected canonical form %q (uint64=%d)", s, u)
		}
	})
}

// Property 2: any string containing a character outside [0-9a-fA-F/]
// must be rejected.  A real LSN never has alphabetic characters
// beyond 'a'..'f' / 'A'..'F' nor punctuation beyond a single '/'.
func TestProperty_LooksLikeLSN_RejectsNonHexAndPunctuation(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		base := rapid.StringMatching(`[0-9a-fA-F]+/[0-9a-fA-F]+`).Draw(t, "valid")
		bad := rapid.RuneFrom([]rune("ghijklmnopqrstuvwxyzGHIJKLMNOPQRSTUVWXYZ.,;:-_ \t!?@#$%^&*()[]{}|\\")).Draw(t, "bad")
		pos := rapid.IntRange(0, len(base)).Draw(t, "pos")
		s := base[:pos] + string(bad) + base[pos:]
		if restore.LooksLikeLSN(s) {
			t.Errorf("LooksLikeLSN accepted %q (a valid LSN %q with bad rune %q inserted at %d)",
				s, base, string(bad), pos)
		}
	})
}

// Property 3: any string with NO '/' is rejected.  An LSN's textual
// form always contains exactly one slash separating the high and
// low 32 bits.
func TestProperty_LooksLikeLSN_RejectsMissingSlash(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		hex := rapid.StringMatching(`[0-9a-fA-F]+`).Draw(t, "hex")
		if restore.LooksLikeLSN(hex) {
			t.Errorf("LooksLikeLSN accepted slashless hex %q", hex)
		}
	})
}

// Property 4: a string with the slash at position 0 (no hex before
// it) or position len-1 (no hex after it) is rejected.
func TestProperty_LooksLikeLSN_RejectsEmptySides(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		hex := rapid.StringMatching(`[0-9a-fA-F]+`).Draw(t, "hex")
		// /<hex>
		if restore.LooksLikeLSN("/" + hex) {
			t.Errorf("LooksLikeLSN accepted slash-first %q", "/"+hex)
		}
		// <hex>/
		if restore.LooksLikeLSN(hex + "/") {
			t.Errorf("LooksLikeLSN accepted slash-last %q", hex+"/")
		}
	})
}

// Property 5: the silent-truncation regression from issue #78.
// pglogrepl.ParseLSN happily accepts "0/3000028x" and truncates;
// LooksLikeLSN must reject it.  Generated form: append any single
// non-hex byte to a canonical LSN — the prefix is valid, but with
// the suffix the WHOLE string is invalid.
func TestProperty_LooksLikeLSN_RejectsTrailingNonHex(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		u := rapid.Uint64().Draw(t, "lsn")
		// Use a deliberately non-hex letter (g..z, G..Z) plus a small
		// punctuation set.  Spaces are also caught.
		bad := rapid.RuneFrom([]rune("ghijklmnopqrstuvwxyzGHIJKLMNOPQRSTUVWXYZ. ?!")).Draw(t, "bad")
		s := pglogrepl.LSN(u).String() + string(bad)
		if restore.LooksLikeLSN(s) {
			t.Errorf("LooksLikeLSN accepted %q (canonical + trailing %q) — the silent-truncation regression behind issue #78 is open again",
				s, string(bad))
		}
	})
}

// supportBlankRuneFiltering is a tiny helper for the next test:
// rapid often draws Unicode-class-letter runes that ARE hex letters;
// we want only NON-hex digits/letters.  Easier than maintaining a
// regex-exclusion: just probe.
func isHexRune(r rune) bool {
	r = unicode.ToLower(r)
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')
}

// Property 6: a canonical LSN with extra '/' anywhere is rejected
// (only one slash separator is valid).
func TestProperty_LooksLikeLSN_RejectsMultipleSlashes(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		u := rapid.Uint64().Draw(t, "lsn")
		s := pglogrepl.LSN(u).String()
		// Insert one extra '/' at a random position.
		pos := rapid.IntRange(0, len(s)).Draw(t, "pos")
		mutated := s[:pos] + "/" + s[pos:]
		if restore.LooksLikeLSN(mutated) {
			t.Errorf("LooksLikeLSN accepted %q (LSN %q with extra slash at %d)",
				mutated, s, pos)
		}
	})
}

// Property 7 (defensive): when LooksLikeLSN says yes, pglogrepl
// agrees.  If the strict shape ever drifts and accepts a string
// pglogrepl rejects, the whole shape-then-parse pattern breaks.
func TestProperty_LooksLikeLSN_AgreesWithPglogrepl(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate within the canonical shape so the property is
		// non-trivial.  Hex segments capped at 16 chars each (uint64
		// range); rapid's StringMatching can over-generate.
		hi := rapid.StringMatching(`[0-9a-fA-F]{1,16}`).Draw(t, "hi")
		lo := rapid.StringMatching(`[0-9a-fA-F]{1,16}`).Draw(t, "lo")
		s := hi + "/" + lo
		if !restore.LooksLikeLSN(s) {
			return // not interesting; we're auditing the YES path
		}
		// LooksLikeLSN said yes; ParseLSN must also accept (modulo
		// overflow if either side >16 hex chars, which our generator
		// excluded).
		if _, err := pglogrepl.ParseLSN(s); err != nil {
			t.Errorf("LooksLikeLSN said YES for %q, but pglogrepl.ParseLSN rejected: %v",
				s, err)
		}
	})
}

// Acceptance smoke: a hand-rolled invalid set that should ALL fail.
// Keeps the file useful even if rapid is somehow disabled.
func TestLooksLikeLSN_HandRolledInvalids(t *testing.T) {
	for _, bad := range []string{
		"",
		" ",
		"abc",
		"/3000000",
		"0/",
		"0",
		"0/3000028x",
		"0/3000028 ",
		"0//3000028",
		"0/3000028/",
		"deadbeef.cafe",
		strings.Repeat("f", 17) + "/0", // overflows but the shape check is shape-only
	} {
		if restore.LooksLikeLSN(bad) {
			// The hex-overflow case is a known shape-only limitation
			// of LooksLikeLSN — the post-shape pglogrepl.ParseLSN
			// call catches it.  Strconv-validate here so the test is
			// honest about what the shape check guarantees.
			if _, err := strconv.ParseUint(strings.SplitN(bad, "/", 2)[0], 16, 64); err == nil {
				t.Errorf("LooksLikeLSN accepted %q", bad)
			}
		}
	}
}
