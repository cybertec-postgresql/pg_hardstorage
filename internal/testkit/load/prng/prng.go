// Package prng provides a deterministic, seeded random number source
// for the testkit's load engine. Determinism is the precondition for
// every meaningful assertion in the testkit ("at 22:00 UTC the
// `users` table had exactly 1,000,000 rows with content digest X");
// without it, "did the restore work?" has no answer.
//
// Implementation: ChaCha8 from the standard library, seeded with a
// 32-byte key derived from the user-supplied uint64 seed. Same seed
// → same byte stream → same generated rows → same digests, today and
// in three years on a different OS / arch.
package prng

import (
	"encoding/binary"
	"math/rand/v2"
)

// New returns a deterministic *rand.Rand seeded with seed. Two calls
// with the same seed return generators that emit the identical byte
// stream — this is the property the testkit relies on.
//
// Use math/rand/v2.ChaCha8 specifically (not the legacy math/rand
// PCG): ChaCha8 is the standard library's cryptographic-quality
// stream cipher, which gives us "no pattern detectable in a 1 TB
// scenario" without us having to reason about PCG state spaces.
func New(seed uint64) *rand.Rand {
	var key [32]byte
	binary.LittleEndian.PutUint64(key[0:8], seed)
	binary.LittleEndian.PutUint64(key[8:16], 0xC0FFEEBEAD42)
	binary.LittleEndian.PutUint64(key[16:24], 0xDEADBEEF12345678)
	binary.LittleEndian.PutUint64(key[24:32], seed^0xA5A5A5A5A5A5A5A5)
	return rand.New(rand.NewChaCha8(key))
}

// Derive returns a sub-PRNG seeded by hashing (parentSeed, label).
// Use it when a scenario phase wants its own deterministic stream
// independent of the parent (so parallel ops reading from the same
// generator don't entangle).
//
// Implementation: a stable mix of seed and a 64-bit hash of label.
// The mix has nothing fancy in it — we just need different labels at
// the same parentSeed to produce different streams, and the same
// (parent, label) pair to always produce the same stream.
func Derive(parentSeed uint64, label string) *rand.Rand {
	h := uint64(14695981039346656037) // FNV-1a 64-bit offset basis
	for i := 0; i < len(label); i++ {
		h ^= uint64(label[i])
		h *= 1099511628211 // FNV prime
	}
	return New(parentSeed ^ h)
}
