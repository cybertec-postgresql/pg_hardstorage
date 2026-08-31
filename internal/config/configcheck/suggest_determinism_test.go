package configcheck

// `config check` tells an operator "unknown key X; did you mean Y?" and
// they act on it. suggest breaks ties with `d < bestD` — strictly less
// — so the FIRST name at the minimum edit distance wins, and the
// substring fallback returns its first match outright. Both "first"
// came from fieldNames, which built its slice by ranging a map, so two
// operators typing the same config typo were told to try two different
// field names, and neither could reproduce the other's output.

import (
	"reflect"
	"testing"
)

func namesFrom(fields ...string) map[string]reflect.StructField {
	out := map[string]reflect.StructField{}
	for _, f := range fields {
		out[f] = reflect.StructField{Name: f}
	}
	return out
}

func TestFieldNames_IsSorted(t *testing.T) {
	got := fieldNames(namesFrom("zulu", "alpha", "mike", "bravo"))
	want := []string{"alpha", "bravo", "mike", "zulu"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// Equidistant candidates: without an order the winner is a coin flip.
func TestSuggest_TiesResolveTheSameWayEveryRun(t *testing.T) {
	// "retentio" is edit distance 1 from both.
	fields := namesFrom("retention", "retentiox", "unrelated_key_name")

	first := suggest("retentio", fieldNames(fields))
	if first == "" {
		t.Fatal("no suggestion produced for a one-character typo")
	}
	for i := 0; i < 300; i++ {
		if got := suggest("retentio", fieldNames(fields)); got != first {
			t.Fatalf("run %d suggested %q, the first run suggested %q — an operator acting on "+
				"this advice cannot reproduce it, and a colleague running the same command "+
				"gets a different answer", i, got, first)
		}
	}
	if first != "retention" {
		t.Errorf("suggestion = %q, want %q (sorted-first among the equidistant candidates)",
			first, "retention")
	}
}

// The substring fallback returns its first match, so it needs the same
// guarantee.
func TestSuggest_SubstringFallbackIsStable(t *testing.T) {
	// No candidate within edit distance 3, but several contain the key.
	fields := namesFrom("zzz_retention_days", "aaa_retention_floor", "mmm_retention_mode")

	first := suggest("retention", fieldNames(fields))
	if first == "" {
		t.Fatal("substring fallback produced nothing")
	}
	for i := 0; i < 300; i++ {
		if got := suggest("retention", fieldNames(fields)); got != first {
			t.Fatalf("run %d: %q != %q (substring fallback depends on iteration order)",
				i, got, first)
		}
	}
	if first != "aaa_retention_floor" {
		t.Errorf("fallback = %q, want %q (sorted-first containing candidate)",
			first, "aaa_retention_floor")
	}
}

// A key with no plausible neighbour must produce no suggestion, or
// `config check` sends operators chasing an unrelated field.
func TestSuggest_NoPlausibleNeighbourSuggestsNothing(t *testing.T) {
	if got := suggest("wildly_different_thing", fieldNames(namesFrom("tls", "worm"))); got != "" {
		t.Errorf("suggested %q for an unrelated key — better to say nothing than to point an "+
			"operator at the wrong setting", got)
	}
}
