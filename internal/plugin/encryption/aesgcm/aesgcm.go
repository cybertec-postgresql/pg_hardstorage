// Package aesgcm implements encryption.Encryptor with AES-256-GCM.
//
// Every Encrypt call draws a fresh 96-bit nonce from crypto/rand.
// nonce_test.go pins that: GCM has NO nonce-misuse resistance, so a
// reused nonce under one key leaks the XOR of the plaintexts and yields
// the authentication subkey, allowing tag forgery. It is catastrophic,
// not degraded.
//
// AEAD authentication: the 16-byte GCM tag is appended to the
// ciphertext by Encrypt and consumed by Decrypt. A failed tag check
// surfaces as encryption.ErrAuthenticationFailed.
//
// # How many messages one key may encrypt
//
// The DEK is NOT per backup. internal/repo/sharedkey mints one shared
// DEK per (deployment, KEK) and every base backup, WAL segment and
// logical CDC batch under that KEKRef reuses it — and `kms rotate`
// re-wraps that same DEK rather than replacing it (replacing it would
// strand chunks the plaintext-hash CAS deduplicates against). So the
// relevant count is every chunk ever encrypted in the repository, for
// its whole life, not one backup's.
//
// With random 96-bit nonces the collision probability after n
// encryptions is about n^2 / 2^97:
//
//	n = 2^32   ~2^-33      NIST SP 800-38D's recommended ceiling for
//	                       random IVs; ~256 TiB of unique post-dedup
//	                       data at the 64 KiB default chunk average
//	n = 2^43.5 ~2^-10      ~768 PiB of unique data
//	n = 2^48   ~1/2        the birthday point — catastrophic
//
// The margin is large even against a repository-lifetime key, which is
// why AES-GCM is acceptable here. Stating the numbers rather than the
// conclusion so a reader can check it against their own scale: an
// earlier version of this comment scoped the bound to "a single
// backup's chunks", which is the wrong denominator for a shared DEK,
// and cited 2^48 as the reassuring figure when 2^48 is the point at
// which collision is roughly even money.
//
// Nothing counts encryptions per DEK. At the volumes above that is a
// defensible omission, not an oversight — but it is an omission, and a
// deployment that concentrates many large clusters under one KEKRef
// shortens the denominator.
//
// # Why AES-GCM and not AES-GCM-SIV
//
// GCM-SIV (RFC 8452) is nonce-misuse resistant and is the SPEC's
// preferred default, but Go's standard library does not ship it and the
// third-party options are unaudited. It is NOT implemented in this
// codebase: encryption.AlgorithmID registers only AlgoNone and
// AlgoAESGCM. The operator-facing docs describe it correctly as
// planned; an earlier version of this comment claimed it already
// shipped "behind an algorithm-selection flag", which was untrue and
// contradicted every other document.
//
// v0.1 backups encrypted with AES-GCM remain readable forever (the
// 24-month forward-compat commitment for the on-disk envelope), so
// adding GCM-SIV later is additive.
package aesgcm

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/encryption"
)

// Encryptor is AES-256-GCM keyed at construction.
type Encryptor struct {
	aead cipher.AEAD
}

// New constructs an Encryptor from a 32-byte key. Returns
// encryption.ErrInvalidKey if the key is the wrong length.
func New(key []byte) (*Encryptor, error) {
	if len(key) != encryption.KeyLen {
		return nil, fmt.Errorf("%w: aes-256-gcm wants %d-byte key, got %d",
			encryption.ErrInvalidKey, encryption.KeyLen, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("aesgcm: NewCipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("aesgcm: NewGCM: %w", err)
	}
	if aead.NonceSize() != encryption.NonceLen {
		return nil, fmt.Errorf("aesgcm: unexpected nonce size %d (want %d)",
			aead.NonceSize(), encryption.NonceLen)
	}
	return &Encryptor{aead: aead}, nil
}

// Name implements encryption.Encryptor.
func (e *Encryptor) Name() string { return "aes-256-gcm" }

// Algorithm implements encryption.Encryptor.
func (e *Encryptor) Algorithm() encryption.AlgorithmID { return encryption.AlgoAESGCM }

// Encrypt seals plaintext with a fresh random nonce. Returns
// (ciphertext, nonce). The ciphertext includes the 16-byte AEAD tag.
func (e *Encryptor) Encrypt(plaintext []byte) ([]byte, [encryption.NonceLen]byte, error) {
	var nonce [encryption.NonceLen]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, nonce, fmt.Errorf("aesgcm: random nonce: %w", err)
	}
	// Seal returns dst || ciphertext || tag; pass nil dst to allocate.
	ct := e.aead.Seal(nil, nonce[:], plaintext, nil)
	return ct, nonce, nil
}

// Decrypt opens ciphertext under nonce. Returns
// encryption.ErrAuthenticationFailed on tag mismatch.
func (e *Encryptor) Decrypt(ciphertext []byte, nonce [encryption.NonceLen]byte) ([]byte, error) {
	pt, err := e.aead.Open(nil, nonce[:], ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", encryption.ErrAuthenticationFailed, err)
	}
	return pt, nil
}
