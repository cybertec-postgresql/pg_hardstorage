package compression_test

// envelope_fuzz_test.go — ReadEnvelope parses the untrusted framing
// every stored chunk carries (version byte, algo, nonce, payload). It
// runs on bytes read straight from the repository — a truncated or
// corrupt object must produce a typed error, never a panic (a slice
// bounds error here crashes restore) and never a payload that outlives
// its own header bounds.

import (
	"bytes"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/compression"
)

func FuzzReadEnvelope(f *testing.F) {
	// Real envelopes as seeds so the fuzzer starts from valid framing
	// and mutates outward.
	f.Add(compression.WriteEnvelopeV1(compression.AlgoZstd, []byte("payload")))
	f.Add(compression.WriteEnvelope(compression.AlgoNone, compression.EncryptionFields{}, []byte("p")))
	f.Add([]byte{})
	f.Add([]byte{0x01})
	f.Add([]byte{0xff, 0xff, 0xff})
	f.Fuzz(func(t *testing.T, b []byte) {
		algo, fields, payload, err := compression.ReadEnvelope(b) // must never panic
		if err != nil {
			return
		}
		// A successful parse must return a payload that is a real
		// sub-slice of the input, never longer than the input.
		if len(payload) > len(b) {
			t.Fatalf("ReadEnvelope returned a %d-byte payload from a %d-byte input",
				len(payload), len(b))
		}
		// Re-framing a parsed-then-reserialised envelope must preserve
		// the payload bytes (framing is lossless for what it accepts).
		if !fields.IsEncrypted() {
			round := compression.WriteEnvelopeV1(algo, payload)
			if _, _, p2, err2 := compression.ReadEnvelope(round); err2 == nil {
				if !bytes.Equal(p2, payload) {
					t.Fatalf("payload not preserved across re-frame: %d vs %d", len(payload), len(p2))
				}
			}
		}
	})
}
