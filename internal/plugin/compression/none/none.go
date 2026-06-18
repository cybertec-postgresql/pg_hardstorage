// Package none is the identity compressor: plaintext is the payload.
// Used as a fallback for tiny chunks (where header overhead exceeds
// gains) and as the canonical "compression: off" choice for tests.
package none

import "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/compression"

// Compressor is the singleton implementation.
type Compressor struct{}

// Name implements compression.Compressor.
func (Compressor) Name() string { return "none" }

// Algorithm implements compression.Compressor.
func (Compressor) Algorithm() compression.AlgorithmID { return compression.AlgoNone }

// Compress returns the input bytes verbatim. The returned algo is
// AlgoNone; the registry decoder must round-trip this through
// Decompress (which is also a no-op) so the contract is symmetric.
func (Compressor) Compress(plaintext []byte) ([]byte, compression.AlgorithmID, error) {
	out := make([]byte, len(plaintext))
	copy(out, plaintext)
	return out, compression.AlgoNone, nil
}

// Decompress returns the input bytes verbatim.
func (Compressor) Decompress(payload []byte) ([]byte, error) {
	out := make([]byte, len(payload))
	copy(out, payload)
	return out, nil
}
