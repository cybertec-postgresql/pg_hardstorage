package integrity

// refs_internal_test.go — the chunk→referrers map is the one structure
// the integrity walk holds for the WHOLE fleet: every distinct chunk
// hash across every manifest, alive until the run finishes. What it
// stores per chunk therefore decides whether a large fleet's audit fits
// in memory, and the entries are invisible in the output (dedupeIDs
// collapses them before anything is reported), so only a direct test
// keeps the invariant honest.

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
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

// localSigner is a package-internal Signer for these tests.
type localSigner struct{ priv ed25519.PrivateKey }

func (s localSigner) Sign(p []byte) []byte         { return ed25519.Sign(s.priv, p) }
func (s localSigner) PublicKey() ed25519.PublicKey { return s.priv.Public().(ed25519.PublicKey) }

// TestVerifyRun_AcceptsLegacyDigestSignature: a run signed BEFORE the
// length-prefix digest fix (delimiter-joined) must still verify —
// changing the signed bytes without a dual-verify would silently
// invalidate every persisted integrity attestation (24-month compat).
// New runs sign with the current digest; verify accepts either.
func TestVerifyRun_AcceptsLegacyDigestSignature(t *testing.T) {
	priv := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	pub := priv.Public().(ed25519.PublicKey)
	signer := localSigner{priv: priv}
	resolver := &SingleKeyResolver{Key: pub}

	r := &Run{
		Schema: SchemaRun, ID: "run-legacy", Status: "completed",
		Strategy:   Strategy{Mode: "content-full"},
		Deployment: "db1",
		Chunks: ChunkSection{Failures: []ChunkFailure{
			{ChunkHash: "h1", Reason: "boom\nwith|delims"},
		}},
	}
	// Sign using the LEGACY canonical bytes, exactly as old code did.
	canon := canonicalRunBytesWith(r, digestFailuresLegacy)
	bh := sha256.Sum256(canon)
	r.BodyHash = fmt.Sprintf("%x", bh[:])
	r.PublicKeyFingerprint = publicKeyFingerprint(pub)
	r.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, canon))

	if err := VerifyRun(r, resolver); err != nil {
		t.Fatalf("a legacy-signed run must still verify after the digest fix: %v", err)
	}

	// A freshly (current-digest) signed run verifies too.
	r2 := &Run{Schema: SchemaRun, ID: "run-new", Status: "completed",
		Strategy: Strategy{Mode: "content-full"}, Deployment: "db1"}
	if err := SignRun(r2, signer); err != nil {
		t.Fatal(err)
	}
	if err := VerifyRun(r2, resolver); err != nil {
		t.Fatalf("current-signed run failed to verify: %v", err)
	}
}
