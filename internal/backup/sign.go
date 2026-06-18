// Package backup carries the manifest schema and the signing primitives
// that turn a pile of chunks into a tamper-evident, restorable backup.
//
// This file: Ed25519 Signer + Verifier + PEM serialization for
// repository-local key files.
//
// Choice of algorithm: Ed25519 ships in stdlib, has small keys (32 bytes
// public, 64 bytes private), small signatures (64 bytes), no curve-pick
// pitfalls, and is fast. We don't need PKI, OCSP, or chain validation
// for the on-repo manifest signature; a single keypair held by the
// agent is sufficient. Cosign / Sigstore integration in the compliance
// slice will layer on top — same Ed25519 underneath.
package backup

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
)

// PEM block types we emit. The "PG_HARDSTORAGE" prefix avoids any chance
// of a generic OpenSSL tool silently mistaking the file for an X.509
// keypair (PKCS#8 / PKIX bytes inside, but the outer header tells humans
// the keys are project-specific).
const (
	pemTypePrivateKey = "PG_HARDSTORAGE ED25519 PRIVATE KEY"
	pemTypePublicKey  = "PG_HARDSTORAGE ED25519 PUBLIC KEY"
)

// SchemeEd25519 is the only signature scheme we support today. The
// Manifest's Attestation.Scheme field carries this string verbatim.
const SchemeEd25519 = "ed25519"

// Signer signs canonical bytes with a private key.
type Signer struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
}

// Verifier verifies a signature against a public key.
type Verifier struct {
	pub ed25519.PublicKey
}

// GenerateKeypair produces a fresh keypair from rand. Use
// rand.Reader from crypto/rand in production; tests can inject a
// deterministic source. Returns the keys in PEM form.
func GenerateKeypair(rnd io.Reader) (privPEM, pubPEM []byte, err error) {
	if rnd == nil {
		rnd = rand.Reader
	}
	pub, priv, err := ed25519.GenerateKey(rnd)
	if err != nil {
		return nil, nil, fmt.Errorf("backup: generate ed25519 keypair: %w", err)
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, nil, fmt.Errorf("backup: marshal private key: %w", err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, nil, fmt.Errorf("backup: marshal public key: %w", err)
	}
	privPEM = pem.EncodeToMemory(&pem.Block{Type: pemTypePrivateKey, Bytes: privDER})
	pubPEM = pem.EncodeToMemory(&pem.Block{Type: pemTypePublicKey, Bytes: pubDER})
	return privPEM, pubPEM, nil
}

// LoadSigner parses a PEM-encoded private key. The corresponding
// public key is derived; no separate file required.
func LoadSigner(privPEM []byte) (*Signer, error) {
	block, _ := pem.Decode(privPEM)
	if block == nil {
		return nil, errors.New("backup: no PEM block in private-key bytes")
	}
	if block.Type != pemTypePrivateKey {
		return nil, fmt.Errorf("backup: PEM block is %q; want %q", block.Type, pemTypePrivateKey)
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("backup: parse private key: %w", err)
	}
	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("backup: not an ed25519 private key (got %T)", key)
	}
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("backup: ed25519 private key did not yield a public key")
	}
	return &Signer{priv: priv, pub: pub}, nil
}

// LoadVerifier parses a PEM-encoded public key.
func LoadVerifier(pubPEM []byte) (*Verifier, error) {
	block, _ := pem.Decode(pubPEM)
	if block == nil {
		return nil, errors.New("backup: no PEM block in public-key bytes")
	}
	if block.Type != pemTypePublicKey {
		return nil, fmt.Errorf("backup: PEM block is %q; want %q", block.Type, pemTypePublicKey)
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("backup: parse public key: %w", err)
	}
	pub, ok := key.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("backup: not an ed25519 public key (got %T)", key)
	}
	return &Verifier{pub: pub}, nil
}

// Sign returns an Ed25519 signature over payload.
func (s *Signer) Sign(payload []byte) []byte {
	return ed25519.Sign(s.priv, payload)
}

// PrivateKey returns the raw ed25519.PrivateKey. Exposed for callers
// that need to drive a third-party Sign-shaped API (e.g. the
// approval package which signs canonical request bytes from outside
// the backup package). The returned value shares memory with the
// Signer's private state — callers must not mutate it.
func (s *Signer) PrivateKey() ed25519.PrivateKey { return s.priv }

// PublicKey returns the raw ed25519.PublicKey. Same caveat as
// PrivateKey: the slice shares memory with the Signer's state.
func (s *Signer) PublicKey() ed25519.PublicKey { return s.pub }

// PublicKeyPEM returns the signer's public key in PEM form. Convenient
// for writing the matching .pub file alongside a freshly-generated
// keypair without re-marshaling.
func (s *Signer) PublicKeyPEM() ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(s.pub)
	if err != nil {
		return nil, fmt.Errorf("backup: marshal public key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: pemTypePublicKey, Bytes: der}), nil
}

// PublicKey returns the raw ed25519.PublicKey this verifier trusts.
// Exposed so callers can use the verifier's key as a trust anchor for
// adjacent artefacts (e.g. anchoring a threshold roster's creator key to
// the same operator key that signs manifests). The slice shares memory
// with the Verifier's state — callers must not mutate it.
func (v *Verifier) PublicKey() ed25519.PublicKey { return v.pub }

// Verify checks signature against payload. Returns nil on success and
// ErrBadSignature on mismatch. Other errors indicate malformed input.
func (v *Verifier) Verify(payload, signature []byte) error {
	if !ed25519.Verify(v.pub, payload, signature) {
		return ErrBadSignature
	}
	return nil
}

// ErrBadSignature is returned by Verify when the signature does not
// match the payload. Use errors.Is to detect.
var ErrBadSignature = errors.New("backup: signature does not verify")
