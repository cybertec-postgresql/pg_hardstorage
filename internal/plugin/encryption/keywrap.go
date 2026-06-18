// keywrap.go — Wrap/Unwrap: AES-256-GCM envelope of a DEK under a KEK with self-contained nonce+tag.
package encryption

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"
)

// WrappedKeyLen is the byte length of the wrapped DEK produced by
// Wrap: 12-byte nonce || 32-byte ciphertext || 16-byte AEAD tag.
const WrappedKeyLen = NonceLen + KeyLen + 16

// Wrap encrypts dek under kek using AES-256-GCM with a random
// nonce. Output layout:
//
//	nonce(12) || ciphertext(32) || tag(16) = 60 bytes
//
// We don't reuse Encryptor.Encrypt for this because key-wrapping
// has different ergonomics: the caller wants a single self-contained
// blob (not a separate ciphertext + nonce pair) so the wrapped value
// can ship in a manifest field as-is.
//
// AAD: empty. We don't bind the wrapped DEK to a specific manifest
// id or backup id — doing so would prevent legitimate cross-manifest
// reads (e.g. the GC pass that walks every manifest's chunks). The
// integrity guarantee we need is "this DEK was wrapped under THIS
// KEK", which the AEAD tag provides without AAD.
func Wrap(kek [KeyLen]byte, dek [KeyLen]byte) ([]byte, error) {
	block, err := aes.NewCipher(kek[:])
	if err != nil {
		return nil, fmt.Errorf("encryption: wrap: aes.NewCipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("encryption: wrap: NewGCM: %w", err)
	}

	out := make([]byte, NonceLen, WrappedKeyLen)
	if _, err := rand.Read(out); err != nil {
		return nil, fmt.Errorf("encryption: wrap: random nonce: %w", err)
	}
	out = aead.Seal(out, out[:NonceLen], dek[:], nil)
	if len(out) != WrappedKeyLen {
		return nil, fmt.Errorf("encryption: wrap: produced %d bytes; want %d",
			len(out), WrappedKeyLen)
	}
	return out, nil
}

// Unwrap decrypts wrapped under kek and returns the DEK. Returns
// ErrAuthenticationFailed when wrapped is the wrong length, when
// the AEAD tag fails, or when kek doesn't match the original wrapping
// KEK. Same we-don't-distinguish-failure-modes posture as Decrypt:
// the caller cannot tell "wrong key" from "tampered bytes" and
// shouldn't try.
func Unwrap(kek [KeyLen]byte, wrapped []byte) ([KeyLen]byte, error) {
	var zero [KeyLen]byte
	if len(wrapped) != WrappedKeyLen {
		return zero, fmt.Errorf("%w: wrapped DEK len=%d, want %d",
			ErrAuthenticationFailed, len(wrapped), WrappedKeyLen)
	}
	block, err := aes.NewCipher(kek[:])
	if err != nil {
		return zero, fmt.Errorf("encryption: unwrap: aes.NewCipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return zero, fmt.Errorf("encryption: unwrap: NewGCM: %w", err)
	}

	nonce := wrapped[:NonceLen]
	ct := wrapped[NonceLen:]
	pt, err := aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return zero, fmt.Errorf("%w: unwrap: %v", ErrAuthenticationFailed, err)
	}
	if len(pt) != KeyLen {
		return zero, fmt.Errorf("encryption: unwrap: plaintext len=%d, want %d",
			len(pt), KeyLen)
	}
	var dek [KeyLen]byte
	copy(dek[:], pt)
	return dek, nil
}

// GenerateDEK draws a 32-byte random key from crypto/rand. Convenience
// for the runner, which generates a fresh DEK for every backup.
func GenerateDEK() ([KeyLen]byte, error) {
	var dek [KeyLen]byte
	if _, err := rand.Read(dek[:]); err != nil {
		return dek, fmt.Errorf("encryption: generate DEK: %w", err)
	}
	return dek, nil
}
