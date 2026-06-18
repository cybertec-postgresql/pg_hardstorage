//go:build mutation_identifier_no_length_cap

package pg

// ValidIdentifier — MUTATED variant.  Drops the 1..63 byte length
// cap (PG's NAMEDATALEN-1) and accepts any-length identifier, even
// the empty string.  An attacker controlling a slot-name field could
// pass a 1000-character string and PG would truncate, but the audit
// surface here would record the full string.
//
// Selected by `go test -tags=mutation_identifier_no_length_cap`.
// The property tests in identifier_property_test.go
// (TestProperty_ValidIdentifier_RejectsOutOfRangeLengths +
// TestValidIdentifier_HandRolledInvalids) must fail under this
// mutation.
//
// See internal/testkit/mutation/registry.go.
func ValidIdentifier(s string) bool {
	// NOTE: deliberately missing both the empty-string AND the >63
	// length guards.
	for i := 0; i < len(s); i++ {
		c := s[i]
		if i == 0 {
			if !(c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
				return false
			}
			continue
		}
		switch {
		case c == '_':
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		default:
			return false
		}
	}
	return true
}
