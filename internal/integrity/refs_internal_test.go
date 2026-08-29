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

// TestDigestFailures_DelimiterInjectionCannotCollide: two DIFFERENT
// failure lists must never produce the same digest, even when a Reason
// carries the '|'/'\n' that the old delimiter-joined encoding used as
// field separators. err.Error() is routinely multi-line, so this is
// reachable with real data; the digest is folded into the SIGNED run,
// so a collision would let the signature commit to the wrong failure
// list.
func TestDigestFailures_DelimiterInjectionCannotCollide(t *testing.T) {
	// List A: one chunk failure whose Reason embeds newlines/pipes
	// that, under "%s|%s|%s\n", would reproduce the byte stream of a
	// two-entry list.
	runA := &Run{
		Chunks: ChunkSection{Failures: []ChunkFailure{
			{ChunkHash: "h1", Reason: "boom\nchunk|h2|other boom"},
		}},
	}
	// List B: two distinct chunk failures whose concatenation under the
	// old join is byte-identical to A's single entry.
	runB := &Run{
		Chunks: ChunkSection{Failures: []ChunkFailure{
			{ChunkHash: "h1", Reason: "boom"},
			{ChunkHash: "h2", Reason: "other boom"},
		}},
	}
	if digestFailures(runA) == digestFailures(runB) {
		t.Fatal("two different failure lists share a digest — the delimiter-injection " +
			"collision is back, and the signed run no longer commits to its failure list")
	}

	// Determinism: identical input, identical digest (order-independent
	// via the internal sort).
	runC := &Run{Chunks: ChunkSection{Failures: []ChunkFailure{
		{ChunkHash: "h2", Reason: "other boom"},
		{ChunkHash: "h1", Reason: "boom"},
	}}}
	if digestFailures(runB) != digestFailures(runC) {
		t.Error("digest is not stable under failure reordering")
	}
}
