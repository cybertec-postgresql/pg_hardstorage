// hash.go — Hash: SHA-256 content-address type with hex-string JSON/YAML marshalling.
package repo

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// Hash is a SHA-256 digest. It is the canonical content-address used
// throughout the repository (chunks, manifest references, audit anchors).
//
// The type matters for two reasons:
//
//  1. Go's encoding/json marshals [32]byte (a fixed-size byte array)
//     as a JSON array of integers — the worst representation for a
//     hash. This type implements TextMarshaler/TextUnmarshaler so it
//     renders as lowercase hex everywhere it appears in JSON or YAML.
//
//  2. It documents intent at API boundaries: a *Hash* parameter is
//     SHA-256 of plaintext, not just any 32-byte buffer.
//
// The underlying type is still [32]byte, so direct conversion works:
//
//	sum := sha256.Sum256(body)   // [32]byte
//	h := repo.Hash(sum)          // explicit convert
//	arr := [32]byte(h)           // and back, when needed
type Hash [32]byte

// HashOf returns the SHA-256 of body as a Hash.
func HashOf(body []byte) Hash {
	return Hash(sha256.Sum256(body))
}

// String returns the lowercase-hex form (the same form MarshalText emits).
func (h Hash) String() string {
	return hex.EncodeToString(h[:])
}

// IsZero reports whether h is the zero hash. Useful as a sentinel.
func (h Hash) IsZero() bool {
	for _, b := range h {
		if b != 0 {
			return false
		}
	}
	return true
}

// MarshalText implements encoding.TextMarshaler. Renders as 64 lowercase
// hex characters. Used by encoding/json and gopkg.in/yaml.v3 when the
// type is a struct field or map value.
func (h Hash) MarshalText() ([]byte, error) {
	out := make([]byte, hex.EncodedLen(len(h)))
	hex.Encode(out, h[:])
	return out, nil
}

// UnmarshalText is the inverse of MarshalText. Accepts exactly 64 hex
// characters; anything else is an error so configuration loaders surface
// typos rather than silently zero-fill.
func (h *Hash) UnmarshalText(b []byte) error {
	want := hex.EncodedLen(len(*h))
	if len(b) != want {
		return fmt.Errorf("hash: bad length %d (want %d hex chars)", len(b), want)
	}
	n, err := hex.Decode(h[:], b)
	if err != nil {
		return fmt.Errorf("hash: decode: %w", err)
	}
	if n != len(*h) {
		return fmt.Errorf("hash: decoded %d bytes (want %d)", n, len(*h))
	}
	return nil
}

// ParseHash is a convenience for the test/CLI side: parses 64 hex chars
// into a Hash, returning an error on bad input.
func ParseHash(s string) (Hash, error) {
	var h Hash
	if err := h.UnmarshalText([]byte(s)); err != nil {
		return Hash{}, err
	}
	return h, nil
}
