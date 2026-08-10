package aesgcm_test

// fuzz_test.go — the AEAD every encrypted chunk rests on.
//
// Two properties fuzzed:
//
//   - Round-trip: Decrypt(Encrypt(x)) == x for all x. A mismatch is
//     silent data loss on an encrypted deployment.
//   - Tamper rejection (the property the whole encryption story
//     depends on): ANY mutation of the ciphertext or nonce must make
//     Decrypt REFUSE with ErrAuthenticationFailed — never return
//     altered-but-accepted plaintext. A fuzzer flipping arbitrary
//     bytes is the direct proof.

import (
	"bytes"
	"errors"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption/aesgcm"
)

func fixedKey() []byte {
	k := make([]byte, encryption.KeyLen)
	for i := range k {
		k[i] = byte(i * 7)
	}
	return k
}

func FuzzAESGCMRoundTrip(f *testing.F) {
	f.Add([]byte(""))
	f.Add([]byte("one chunk of plaintext"))
	f.Add(bytes.Repeat([]byte{0x00}, 4096))
	f.Fuzz(func(t *testing.T, plaintext []byte) {
		e, err := aesgcm.New(fixedKey())
		if err != nil {
			t.Fatal(err)
		}
		ct, nonce, err := e.Encrypt(plaintext)
		if err != nil {
			t.Fatalf("Encrypt %d bytes: %v", len(plaintext), err)
		}
		got, err := e.Decrypt(ct, nonce)
		if err != nil {
			t.Fatalf("Decrypt of our own ciphertext failed: %v", err)
		}
		if !bytes.Equal(got, plaintext) {
			t.Fatalf("ROUND-TRIP MISMATCH: in=%d out=%d — a restored encrypted chunk would "+
				"not equal what was backed up", len(plaintext), len(got))
		}
	})
}

func FuzzAESGCMTamperRejected(f *testing.F) {
	// The fuzzer supplies (plaintext, a byte index, a xor mask); we
	// encrypt, mutate one byte of the ciphertext-or-nonce at that
	// index, and require Decrypt to REFUSE. AEAD guarantees this; the
	// fuzz proves the wiring honours the guarantee for every position.
	f.Add([]byte("secret chunk contents"), 0, byte(1))
	f.Add([]byte("secret chunk contents"), 5, byte(0xff))
	f.Fuzz(func(t *testing.T, plaintext []byte, idx int, mask byte) {
		if mask == 0 {
			mask = 1 // a no-op xor would be a false "accepted"
		}
		e, err := aesgcm.New(fixedKey())
		if err != nil {
			t.Fatal(err)
		}
		ct, nonce, err := e.Encrypt(plaintext)
		if err != nil {
			t.Fatal(err)
		}
		// Flatten ciphertext||nonce into one mutation space so a
		// flipped bit anywhere in the sealed blob is exercised.
		blob := append(append([]byte{}, ct...), nonce[:]...)
		if len(blob) == 0 {
			return
		}
		pos := ((idx % len(blob)) + len(blob)) % len(blob)
		blob[pos] ^= mask

		mutCT := blob[:len(ct)]
		var mutNonce [encryption.NonceLen]byte
		copy(mutNonce[:], blob[len(ct):])

		got, derr := e.Decrypt(mutCT, mutNonce)
		if derr == nil {
			t.Fatalf("TAMPER ACCEPTED: a one-byte mutation at position %d of "+
				"ciphertext||nonce decrypted WITHOUT error to %d bytes — the AEAD "+
				"authentication guarantee is not being honoured, and corrupted or forged "+
				"chunks would restore silently", pos, len(got))
		}
		if !errors.Is(derr, encryption.ErrAuthenticationFailed) {
			t.Fatalf("tamper rejected but with the wrong error type: %v (want "+
				"ErrAuthenticationFailed)", derr)
		}
	})
}
