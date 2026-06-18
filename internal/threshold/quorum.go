//go:build !mutation_threshold_off_by_one

package threshold

// quorumMet reports whether `members` distinct valid signatures
// meet the configured `threshold`.  Inclusive comparison: exactly
// k signatures DO meet a k-of-n requirement.
//
// Mutation-testing note: a deliberately-broken variant lives in
// quorum_mutation_threshold_off_by_one.go (selected by
// `go test -tags=mutation_threshold_off_by_one`) that uses `>`
// instead of `>=` — the classic off-by-one in quorum logic.  The
// test suite must catch it.  See internal/testkit/mutation.
func quorumMet(members, threshold int) bool {
	return members >= threshold
}
