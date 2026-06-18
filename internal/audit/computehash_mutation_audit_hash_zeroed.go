//go:build mutation_audit_hash_zeroed

package audit

// ComputeHash — MUTATED variant.  Always returns a constant zero
// hash.  Selected by `go test -tags=mutation_audit_hash_zeroed`.
//
// This is the most aggressive possible mutation of a hash function:
// chain-walking tests must fail hard because every event's PrevHash
// no longer matches its predecessor's Hash (or rather, every event
// has the same Hash, which collapses the chain).  Tests that
// validate chain integrity (TestVerifyChain*, TestComputeHash_*)
// must surface this as a verification failure.
//
// See internal/testkit/mutation/registry.go for the harness that
// runs `go test -tags=mutation_audit_hash_zeroed` and asserts the
// suite fails.
func ComputeHash(ev *Event) (string, error) {
	const zero = "0000000000000000000000000000000000000000000000000000000000000000"
	return zero, nil
}
