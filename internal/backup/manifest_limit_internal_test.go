package backup

import (
	"strings"
	"testing"
)

// TestReadAllLimited pins input-validation audit #2: a manifest read is
// bounded, so an oversized/malformed object errors instead of being slurped
// unboundedly into memory by io.ReadAll. The production manifest read sites
// route through this helper with MaxManifestBytes.
func TestReadAllLimited(t *testing.T) {
	// Under the limit: returns the bytes verbatim.
	body, err := readAllLimited(strings.NewReader("hello"), 100)
	if err != nil || string(body) != "hello" {
		t.Fatalf("under-limit read: body=%q err=%v", body, err)
	}

	// Exactly at the limit: still ok.
	if _, err := readAllLimited(strings.NewReader("12345"), 5); err != nil {
		t.Errorf("at-limit read should succeed; got %v", err)
	}

	// Over the limit: error, no unbounded allocation.
	if _, err := readAllLimited(strings.NewReader(strings.Repeat("x", 10_000)), 16); err == nil {
		t.Fatal("over-limit read must error (input-validation audit #2); got nil")
	} else if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("over-limit error should mention the byte limit; got %v", err)
	}
}

// TestMaxManifestBytes_IsBounded: the production cap is finite and generous
// (a sanity guard so a refactor can't accidentally set it to 0/negative,
// which would reject every manifest, or leave it effectively unbounded).
func TestMaxManifestBytes_IsBounded(t *testing.T) {
	if MaxManifestBytes <= 0 {
		t.Fatalf("MaxManifestBytes must be positive; got %d", MaxManifestBytes)
	}
	if MaxManifestBytes > 1<<33 { // 8 GiB — anything larger is effectively unbounded
		t.Errorf("MaxManifestBytes = %d is too large to be a meaningful bound", MaxManifestBytes)
	}
}
