package repo

import (
	"strings"
	"testing"
)

// TestReadEnvelopeLimited pins input-validation audit #3: GetChunkBytes
// reads a chunk envelope through a bounded reader, so an oversized or
// malformed envelope errors instead of being slurped unboundedly into
// memory by io.ReadAll.
func TestReadEnvelopeLimited(t *testing.T) {
	body, err := readEnvelopeLimited(strings.NewReader("chunk"), 100)
	if err != nil || string(body) != "chunk" {
		t.Fatalf("under-limit read: body=%q err=%v", body, err)
	}
	if _, err := readEnvelopeLimited(strings.NewReader("12345"), 5); err != nil {
		t.Errorf("at-limit read should succeed; got %v", err)
	}
	if _, err := readEnvelopeLimited(strings.NewReader(strings.Repeat("x", 10_000)), 16); err == nil {
		t.Fatal("over-limit envelope must error (input-validation audit #3); got nil")
	} else if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("over-limit error should mention the byte limit; got %v", err)
	}
}

func TestMaxChunkEnvelopeBytes_IsBounded(t *testing.T) {
	if MaxChunkEnvelopeBytes <= 0 || MaxChunkEnvelopeBytes > 1<<33 {
		t.Errorf("MaxChunkEnvelopeBytes = %d is not a meaningful finite bound", MaxChunkEnvelopeBytes)
	}
}
