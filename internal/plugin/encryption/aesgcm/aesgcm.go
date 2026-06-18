// Package aesgcm implements encryption.Encryptor with AES-256-GCM.
//
// Every Encrypt call draws a fresh 96-bit nonce from crypto/rand. The
// AES-GCM nonce-uniqueness invariant therefore holds with
// overwhelming probability up to ~2^32 messages per key (NIST's
// recommendation; we never approach that bound for a single backup's
// chunks even at 100+ TB scale).
//
// AEAD authentication: the 16-byte GCM tag is appended to the
// ciphertext by Encrypt and consumed by Decrypt. A failed tag check
// surfaces as encryption.ErrAuthenticationFailed.
//
// Why AES-GCM and not AES-GCM-SIV (the spec's preferred default)?
//
//   - GCM-SIV (RFC 8452) provides nonce-misuse resistance — accidental
//     nonce reuse degrades only the affected messages rather than
//     leaking the key. That property is valuable, but Go's standard
//     library doesn't ship a GCM-SIV implementation. The third-party
//     options we'd vendor are small but unaudited; for a v0.1 we
//     prefer the stdlib path.
//
//   - With crypto/rand-derived nonces, the practical risk of nonce
//     reuse is essentially zero (birthday bound at 2^48 per key for
//     96-bit nonces). The spec's GCM-SIV preference is a defence-
//     in-depth measure, not a correctness requirement.
//
// GCM-SIV ships alongside the AWS KMS plugin, behind an
// algorithm-selection flag. v0.1 backups encrypted with AES-GCM
// remain readable forever (24-month forward-compat commitment for
// the on-disk envelope).
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
