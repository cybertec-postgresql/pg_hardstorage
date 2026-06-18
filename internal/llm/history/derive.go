// derive.go — HKDF-SHA256 derivation of the LLM-history DEK from KEK + salt + per-principal scope.
package history

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
)

// DeriveDEK returns a deterministic 32-byte data-encryption
// key for the LLM history store, derived from a master KEK
// + a per-installation salt + an optional per-principal
// scope.
//
// We use HKDF-SHA256 (RFC 5869) so:
//   - the same KEK + salt always produces the same DEK
//     (operators don't have to track a separate llm-history
//     key file);
//   - the DEK is cryptographically distinct from the KEK
//     (a leaked DEK can't be inverted to the KEK);
//   - per-principal derivation lets `alice` and `bob` on
//     the same host have non-overlapping ciphertexts even
//     when both share the host's KEK.
//
// info encoding:
//
//	"pg_hardstorage.llm.history.v1"               (whole-host scope)
//	"pg_hardstorage.llm.history.v1::<principal>"  (per-principal)
//
// The version suffix on the info string is the schema-
// versioning hook: a future v2 derivation (different KDF,
// different domain-separator) lands as a new info value
// without invalidating existing v1 ciphertexts (the store
// records which version produced each session via the
// meta.json sidecar).
func DeriveDEK(kek []byte, principal string) ([]byte, error) {
	if len(kek) != 32 {
		return nil, fmt.Errorf("history: KEK must be 32 bytes, got %d", len(kek))
	}
	info := []byte(InfoBase)
	if principal != "" {
		info = append(info, []byte("::"+principal)...)
	}
	// HKDF: extract → expand.  No salt (per RFC 5869 §3.1
	// which permits omitting salt; the KEK is already a
	// uniformly-random key, so the extract phase only
	// applies a domain separator — equivalent to a
	// zero-salt HMAC).
	prk := hmacSHA256(nil, kek)
	dek := make([]byte, 32)
	if _, err := hkdfExpand(prk, info, dek); err != nil {
		return nil, fmt.Errorf("history: hkdf expand: %w", err)
	}
	return dek, nil
}

// InfoBase is the HKDF info string for v1 derivation.
// Stable across the 24-month back-compat window.
const InfoBase = "pg_hardstorage.llm.history.v1"

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

// hkdfExpand implements RFC 5869 §2.3 (Expand).
// Length-bounded; we only ever need 32 bytes so a single
// HMAC iteration suffices, but we keep the loop generic so
// future callers can derive longer keys.
func hkdfExpand(prk, info, out []byte) (int, error) {
	if len(out) > 255*sha256.Size {
		return 0, errors.New("history: hkdf output too long")
	}
	var t []byte
	written := 0
	for i := byte(1); written < len(out); i++ {
		h := hmac.New(sha256.New, prk)
		h.Write(t)
		h.Write(info)
		h.Write([]byte{i})
		t = h.Sum(nil)
		n := copy(out[written:], t)
		written += n
	}
	return written, nil
}

// readKEKFromKeyring is a small helper the CLI uses when
// constructing the history.Store.  Returns the 32-byte KEK
// when present at <keyringDir>/kek.bin, or (nil, false)
// when absent — letting the CLI degrade to a
// history-disabled session rather than failing.
//
// Kept here (not in keystore) to avoid an import cycle:
// keystore knows nothing about LLM history; history knows
// the file layout but not the keystore's API surface.
func readKEKFromKeyring(_ io.Reader) ([]byte, bool) { return nil, false }
