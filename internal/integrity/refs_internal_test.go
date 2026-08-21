package integrity

// refs_internal_test.go — the chunk→referrers map is the one structure
// the integrity walk holds for the WHOLE fleet: every distinct chunk
// hash across every manifest, alive until the run finishes. What it
// stores per chunk therefore decides whether a large fleet's audit fits
// in memory, and the entries are invisible in the output (dedupeIDs
// collapses them before anything is reported), so only a direct test
// keeps the invariant honest.

import (
	"strings"
	"testing"
)

func TestAppendRef_OneEntryPerDistinctReferrer(t *testing.T) {
	var refs []string
	// One manifest referencing the same chunk 10k times — a repeated
	// all-zeroes page in a big relation.
	for range 10_000 {
		refs = appendRef(refs, "db1.full.20260501T120000Z")
	}
	if len(refs) != 1 {
		t.Fatalf("len(refs) = %d after 10k references from one backup; want 1", len(refs))
	}

	// A second manifest referencing the same chunk is a genuinely new
	// referrer and must be recorded.
	refs = appendRef(refs, "db1.incr.20260502T120000Z")
	refs = appendRef(refs, "db1.incr.20260502T120000Z")
	if len(refs) != 2 {
		t.Fatalf("len(refs) = %d; want 2 (one per distinct backup)", len(refs))
	}
	if refs[0] == refs[1] {
		t.Fatalf("both entries are %q; distinct referrers collapsed", refs[0])
	}
}

// TestAppendRef_FeedsDedupeIDs: whatever appendRef keeps must still
// render the same operator-facing ReferencedBy list.
func TestAppendRef_FeedsDedupeIDs(t *testing.T) {
	var refs []string
	for _, id := range []string{"b.a", "b.a", "b.b", "b.b", "b.c"} {
		refs = appendRef(refs, id)
	}
	got := strings.Join(dedupeIDs(refs), ",")
	if want := "b.a,b.b,b.c"; got != want {
		t.Errorf("dedupeIDs = %q; want %q", got, want)
	}
}
