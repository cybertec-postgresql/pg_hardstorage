//go:build mutation_threshold_off_by_one

package threshold

// quorumMet — MUTATED variant.  Uses strict `>` instead of `>=`,
// so a roster requiring exactly k of n signatures with k
// signatures collected returns false.  The classic off-by-one in
// quorum logic.
//
// Selected by `go test -tags=mutation_threshold_off_by_one`.  Any
// test that asserts a 2-of-2 or 1-of-1 attestation passes the gate
// must fail under this mutation; specifically
// TestVerifyAttestation_QuorumMet (with members == threshold).
//
// See internal/testkit/mutation/registry.go.
func quorumMet(members, threshold int) bool {
	return members > threshold
}
