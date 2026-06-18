//go:build mutation_lsn_shape_loose

package restore

import "strings"

// LooksLikeLSN — MUTATED variant.  Drops the "exactly one slash"
// check, re-introducing the regression the property test caught:
// "0//0" / "0//3000028" / "0/3000028/" silently pass the shape gate
// even though they cannot be valid LSNs.
//
// Selected by `go test -tags=mutation_lsn_shape_loose`.  The
// property test in recovery_property_test.go
// (TestProperty_LooksLikeLSN_RejectsMultipleSlashes plus the
// hand-rolled smoke) must fail under this mutation.
//
// See internal/testkit/mutation/registry.go.
func LooksLikeLSN(s string) bool {
	i := strings.IndexByte(s, '/')
	if i <= 0 || i == len(s)-1 {
		return false
	}
	// NOTE: the strict "no second slash" check is intentionally
	// missing here.  The for-loop below `continue`s on every '/',
	// so a string like "0//0" walks every rune as legal hex with
	// the slashes silently skipped.
	for _, b := range s {
		if b == '/' {
			continue
		}
		switch {
		case b >= '0' && b <= '9':
		case b >= 'a' && b <= 'f':
		case b >= 'A' && b <= 'F':
		default:
			return false
		}
	}
	return true
}
