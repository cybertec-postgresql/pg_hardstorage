package aesgcm_test

// Nonce freshness is the property AES-GCM's security rests on. Reusing
// a nonce under one key is catastrophic, not degraded: it leaks the XOR
// of the two plaintexts and hands an attacker the authentication
// subkey, so tags for arbitrary messages can be forged. GCM has no
// misuse resistance — that is exactly what GCM-SIV was designed to add,
// and GCM-SIV is not implemented here.
//
// Nothing asserted it. The fuzz tests cover round-trip and
// tamper-rejection, and both still pass if Encrypt returns a constant
// nonce.
//
// This codebase has a specific reason to get it wrong. The CAS
// deduplicates by PLAINTEXT hash, and a reasonable-sounding step from
// there is "identical plaintext should produce identical ciphertext, so
// derive the nonce from the plaintext". That change would look like a
// dedup improvement, keep every existing test green, and destroy the
// confidentiality and integrity of every chunk sharing a DEK — which,
// because the DEK is shared per (deployment, KEK) across backups, WAL
// and logical CDC for the life of the repository, is all of them.

import (
	"bytes"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption/aesgcm"
)

func newEnc(t *testing.T) *aesgcm.Encryptor {
	t.Helper()
	key := make([]byte, encryption.KeyLen)
	for i := range key {
		key[i] = byte(i)
	}
	e, err := aesgcm.New(key)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// The regression that matters: same key, same plaintext, twice.
func TestEncrypt_SamePlaintextGetsAFreshNonceAndDifferentCiphertext(t *testing.T) {
	e := newEnc(t)
	pt := []byte("the same chunk bytes, encrypted twice")

	ct1, n1, err := e.Encrypt(pt)
	if err != nil {
		t.Fatal(err)
	}
	ct2, n2, err := e.Encrypt(pt)
	if err != nil {
		t.Fatal(err)
	}

	if n1 == n2 {
		t.Fatalf("two Encrypt calls on the same plaintext reused nonce %x — under one key "+
			"that leaks the XOR of the plaintexts and enables tag forgery for arbitrary "+
			"messages. GCM has no misuse resistance; if this was a deliberate change to make "+
			"encryption deterministic for dedup, it must be reverted: the CAS already "+
			"deduplicates by PLAINTEXT hash and does not need identical ciphertext.", n1)
	}
	if bytes.Equal(ct1, ct2) {
		t.Error("identical ciphertext for identical plaintext — encryption is deterministic, " +
			"which means the nonce is not fresh")
	}
}

// Freshness across many calls, not just two.
func TestEncrypt_NoncesDoNotRepeatAcrossManyCalls(t *testing.T) {
	e := newEnc(t)
	const n = 20000
	seen := make(map[[encryption.NonceLen]byte]int, n)
	for i := 0; i < n; i++ {
		_, nonce, err := e.Encrypt([]byte("chunk"))
		if err != nil {
			t.Fatal(err)
		}
		if prev, dup := seen[nonce]; dup {
			t.Fatalf("nonce %x repeated at calls %d and %d — a 96-bit random nonce must not "+
				"collide in %d draws; this is a counter or a constant, not crypto/rand",
				nonce, prev, i, n)
		}
		seen[nonce] = i
	}
}

// The nonce must be the full 96 bits. A nonce that is mostly zero
// bytes collides far sooner than the birthday bound suggests, and the
// bound is the entire safety argument.
func TestEncrypt_NonceUsesTheFullWidth(t *testing.T) {
	e := newEnc(t)
	var orAll, andAll [encryption.NonceLen]byte
	for i := range andAll {
		andAll[i] = 0xFF
	}
	for i := 0; i < 2000; i++ {
		_, nonce, err := e.Encrypt([]byte("x"))
		if err != nil {
			t.Fatal(err)
		}
		for j := range nonce {
			orAll[j] |= nonce[j]
			andAll[j] &= nonce[j]
		}
	}
	for j := range orAll {
		if orAll[j] != 0xFF {
			t.Errorf("nonce byte %d never had all bits set across 2000 draws (OR=%02x) — the "+
				"nonce is not full-width random, so the birthday bound the safety argument "+
				"rests on does not apply", j, orAll[j])
		}
		if andAll[j] != 0x00 {
			t.Errorf("nonce byte %d never had a bit clear across 2000 draws (AND=%02x)",
				j, andAll[j])
		}
	}
	if encryption.NonceLen != 12 {
		t.Errorf("NonceLen = %d, want 12 (96-bit, the width GCM's random-nonce bound assumes)",
			encryption.NonceLen)
	}
}

// Decrypt must bind to the exact nonce: the nonce is carried in the
// envelope beside the ciphertext, so a swapped or corrupted one has to
// fail rather than silently return wrong plaintext.
func TestDecrypt_WrongNonceIsRejected(t *testing.T) {
	e := newEnc(t)
	pt := []byte("payload")
	ct, nonce, err := e.Encrypt(pt)
	if err != nil {
		t.Fatal(err)
	}
	other := nonce
	other[0] ^= 0x01
	if _, err := e.Decrypt(ct, other); err == nil {
		t.Fatal("Decrypt accepted a ciphertext under the wrong nonce")
	}
	// And the right one still works, so the test above is not passing
	// because Decrypt rejects everything.
	got, err := e.Decrypt(ct, nonce)
	if err != nil {
		t.Fatalf("Decrypt with the correct nonce failed: %v", err)
	}
	if !bytes.Equal(got, pt) {
		t.Errorf("round trip = %q, want %q", got, pt)
	}
}
