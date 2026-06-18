//go:build !mutation_audit_hash_zeroed

package audit

import (
	"crypto/sha256"
	"encoding/hex"
)

// ComputeHash returns the SHA-256 hash of the event's canonical JSON
// (with Hash zeroed). The result is hex-encoded and 64 chars long.
//
// Mutating ev.PrevHash before ComputeHash IS the chain-link operation:
// the event's hash bakes in the prior hash, so changing any historical
// event's bytes cascades through every subsequent hash.
//
// Mutation-testing note: a deliberately-broken variant lives in
// computehash_mutation_audit_hash_zeroed.go and is selected by
// `go test -tags=mutation_audit_hash_zeroed`.  See
// internal/testkit/mutation.
func ComputeHash(ev *Event) (string, error) {
	body, err := canonicalForHash(ev)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}
