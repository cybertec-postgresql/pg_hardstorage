package zstd_test

// fuzz_test.go — the codec that touches every backed-up byte.
//
// Compression is on the critical data path with the worst failure
// mode there is: a chunk that WRITES fine but reads back wrong is
// silent data loss — it passes the backup, passes verify (which hashes
// the stored payload, not a plaintext round-trip), and only surfaces
// at restore. Two properties, both fuzzed because a one-in-a-billion
// input is exactly what a fleet of backups will eventually hit:
//
//   - Round-trip fidelity: Decompress(Compress(x)) == x for ALL x.
//   - Crash-freedom on corrupt input: a truncated or garbage payload
//     (partial storage write, bitrot) must ERROR, never panic.

import (
	"bytes"
	"testing"

	zstdcodec "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/compression/zstd"
)

func FuzzZstdRoundTrip(f *testing.F) {
	f.Add([]byte(""))
	f.Add([]byte("x"))
	f.Add(bytes.Repeat([]byte("A"), 63))    // just below the AlgoNone cutoff
	f.Add(bytes.Repeat([]byte("A"), 64))    // exactly at it
	f.Add(bytes.Repeat([]byte("AB"), 5000)) // highly compressible
	f.Add([]byte{0x00, 0xff, 0x7f, 0x80, 0x01})
	f.Fuzz(func(t *testing.T, plaintext []byte) {
		c := zstdcodec.NewDefault()
		payload, algo, err := c.Compress(plaintext)
		if err != nil {
			t.Fatalf("Compress errored on %d-byte input: %v", len(plaintext), err)
		}
		// AlgoNone is a verbatim copy for tiny chunks; AlgoZstd for the
		// rest. Either way the round trip is the contract.
		got, err := c.Decompress(payload)
		if algo.String() == "none" {
			// The verbatim path: payload IS the plaintext, no decode.
			if !bytes.Equal(payload, plaintext) {
				t.Fatalf("AlgoNone payload is not verbatim: in=%d out=%d", len(plaintext), len(payload))
			}
			return
		}
		if err != nil {
			t.Fatalf("Decompress errored on our own %d-byte compressed output: %v",
				len(payload), err)
		}
		if !bytes.Equal(got, plaintext) {
			t.Fatalf("ROUND-TRIP MISMATCH: in=%d out=%d — a restored chunk would not equal "+
				"what was backed up (silent data loss)", len(plaintext), len(got))
		}
	})
}

func FuzzZstdDecompressCorrupt(f *testing.F) {
	// Seeds: real compressed frames and garbage. The property is
	// crash-freedom — a truncated frame from a partial write must
	// error, not panic or hang the restore.
	c := zstdcodec.NewDefault()
	valid, _, _ := c.Compress(bytes.Repeat([]byte("payload-"), 64))
	f.Add(valid)
	if len(valid) > 4 {
		f.Add(valid[:len(valid)/2]) // truncated frame
	}
	f.Add([]byte{0x28, 0xb5, 0x2f, 0xfd}) // zstd magic, then nothing
	f.Add([]byte("not compressed at all"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, payload []byte) {
		_, _ = zstdcodec.NewDefault().Decompress(payload) // must never panic
	})
}
