//go:build integration

package testkit

import "testing"

// TestExpectedPGMajor_NoSilentSubstitution: the helper must never run a
// different major than the one requested. The previous allowlist
// coerced 19 (and any typo) to 17 silently — a matrix believing it
// tested PG 19 was testing 17 under a 19 label.
func TestExpectedPGMajor_NoSilentSubstitution(t *testing.T) {
	for _, v := range []string{"15", "16", "17", "18", "19", "20"} {
		t.Setenv("PG_HARDSTORAGE_TEST_PG_MAJOR", v)
		if got := ExpectedPGMajor(); got != v {
			t.Errorf("ExpectedPGMajor() = %q with env %q — silent substitution", got, v)
		}
	}
	t.Setenv("PG_HARDSTORAGE_TEST_PG_MAJOR", "")
	if got := ExpectedPGMajor(); got != "17" {
		t.Errorf("unset env: got %q, want default 17", got)
	}
	t.Setenv("PG_HARDSTORAGE_TEST_PG_MAJOR", "banana")
	defer func() {
		if recover() == nil {
			t.Error("a non-numeric major did not panic; it would silently run some default " +
				"while claiming to be banana")
		}
	}()
	_ = ExpectedPGMajor()
}
