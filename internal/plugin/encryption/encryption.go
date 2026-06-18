// Package encryption defines the per-chunk encryption contract.
//
// On-disk envelope (chunk-level, written by the CAS):
//
//	[1] EnvelopeVersion (0x01 = compression-only, 0x02 = compression+encryption)
//	[1] CompressionAlgo
//	... v0x02 only:
//	[1] EncryptionAlgo
//	[12] Nonce
//	[N] payload
//
// For v0x02 with EncryptionAlgo != AlgoNone, the payload IS the
// ciphertext (with AEAD tag appended per the codec). For
// EncryptionAlgo == AlgoNone (or v0x01), the payload is the
// (possibly compressed) plaintext directly.
//
// Threat model:
//
//   - Repo storage is treated as untrusted. An attacker with read-only
//     access to chunks should not be able to recover plaintext, even
//     if they observe many chunks across many backups.
//
//   - The KEK is the trust root. Compromise of the KEK = compromise
//     of every backup it wraps a DEK for. Operators are responsible
//     for KEK custody (file-system permissions, KMS, HSM as a future
//     option).
//
//   - We do NOT defend against an attacker with write access to the
//     repo. They can corrupt or delete chunks; integrity verification
//     (the SHA-256 round-trip in CAS.GetChunkBytes) catches that as
//     ErrChecksumMismatch, but doesn't prevent denial-of-service.
//
// Encryption is OPTIONAL at the CAS level: the CAS works correctly
// either with or without an Encryptor. Per-backup choice (controlled
// by the runner) means a single repo can hold a mix of encrypted and
// unencrypted backups, which is exactly the migration story the SPEC
// promises.
//
// v0.1 ships AES-256-GCM with a random 96-bit nonce per chunk.
// AES-256-GCM-SIV (RFC 8452, the spec's preferred default) requires
// a third-party library; that and AWS KMS support are+.
package encryption

import (
	"errors"
	"fmt"
)

// AlgorithmID enumerates encryption codecs. Stable across releases —
// the byte value goes on disk and into the 24-month backward-read
// commitment.
type AlgorithmID byte

const (
	// AlgoNone means "no encryption applied." Used for chunks
	// committed by an unencrypted backup.
	AlgoNone AlgorithmID = 0

	// AlgoAESGCM is AES-256-GCM with a 96-bit (12-byte) random nonce
	// per chunk, 128-bit GCM authentication tag.
	AlgoAESGCM AlgorithmID = 1
)

// String returns the canonical lowercase name. Used in manifests
// and event bodies.
func (a AlgorithmID) String() string {
	switch a {
	case AlgoNone:
		return "none"
	case AlgoAESGCM:
		return "aes-256-gcm"
	default:
		return fmt.Sprintf("unknown-encryption-algo-%d", a)
	}
}

// NonceLen is the byte length of nonces in the on-disk envelope.
// AES-GCM uses 96 bits; the envelope reserves 12 bytes regardless of
// codec so the on-disk frame is fixed-shape.
const NonceLen = 12

// Encryptor is the codec contract.
//
// Encrypt takes plaintext and returns (ciphertext, nonce). The
// codec is responsible for nonce uniqueness — typically by drawing
// from crypto/rand.Reader.
//
// Decrypt takes (ciphertext, nonce) and returns plaintext. AEAD
// authentication failure is reported as ErrAuthenticationFailed
// (wrapped with codec details).
type Encryptor interface {
	Name() string
	Algorithm() AlgorithmID
	Encrypt(plaintext []byte) (ciphertext []byte, nonce [NonceLen]byte, err error)
	Decrypt(ciphertext []byte, nonce [NonceLen]byte) (plaintext []byte, err error)
}

// CodecRegistry resolves AlgorithmID -> Encryptor for the read path.
// Same shape as the compression registry. Encryptors that use
// per-instance keys (every real codec) re-register under each backup
// when the runner builds the CAS — registry isn't a singleton in
// practice; it's a per-backup lookup table.
type CodecRegistry struct {
	codecs map[AlgorithmID]Encryptor
}

// NewRegistry returns an empty registry.
func NewRegistry() *CodecRegistry {
	return &CodecRegistry{codecs: map[AlgorithmID]Encryptor{}}
}

// Register installs c for algo. Panics on double-registration.
func (r *CodecRegistry) Register(algo AlgorithmID, c Encryptor) {
	if _, ok := r.codecs[algo]; ok {
		panic(fmt.Sprintf("encryption: algorithm %d already registered", algo))
	}
	r.codecs[algo] = c
}

// Lookup returns the Encryptor for algo, or ErrUnknownAlgorithm.
func (r *CodecRegistry) Lookup(algo AlgorithmID) (Encryptor, error) {
	c, ok := r.codecs[algo]
	if !ok {
		return nil, fmt.Errorf("%w: %d", ErrUnknownAlgorithm, algo)
	}
	return c, nil
}

// Has reports whether algo is registered.
func (r *CodecRegistry) Has(algo AlgorithmID) bool {
	_, ok := r.codecs[algo]
	return ok
}

// ErrAuthenticationFailed is returned by Decrypt when the AEAD tag
// doesn't match. Indicates either ciphertext corruption or a wrong
// key; the caller cannot tell them apart (and shouldn't try to —
// distinguishing them leaks a side channel).
var ErrAuthenticationFailed = errors.New("encryption: authentication failed")

// ErrUnknownAlgorithm means the envelope's algo byte refers to a
// codec we don't recognise. The failure mode for "this repo has
// chunks written by a future pg_hardstorage with a new encryption
// algorithm we don't ship."
var ErrUnknownAlgorithm = errors.New("encryption: unknown algorithm")

// ErrInvalidKey is returned by codec constructors when the supplied
// key isn't the right length / shape.
var ErrInvalidKey = errors.New("encryption: invalid key")

// KeyLen is the byte length of the symmetric keys we use throughout
// (DEKs, KEKs, derived chunk keys). 32 bytes = 256 bits, which is
// what AES-256-GCM needs. We standardise on this length so key-
// management code (wrapping, KDFs) never has to special-case sizes.
const KeyLen = 32
