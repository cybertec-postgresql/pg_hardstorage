//go:build !mutation_identifier_no_length_cap

// identifier.go — ValidIdentifier: minimal PG-identifier shape check.
//
// Mutation-testing note: a deliberately-broken variant lives in
// identifier_mutation_no_length_cap.go (selected by
// `go test -tags=mutation_identifier_no_length_cap`) that drops the
// 1..63 byte length cap.  The property tests in
// identifier_property_test.go must fail under the tag.  See
// internal/testkit/mutation/registry.go.
package pg

// ValidIdentifier reports whether s matches the PostgreSQL unquoted-
// identifier rule used for replication slots, publications, roles,
// schemas, and table names:
//
//   - first byte: letter [a-zA-Z] or underscore
//   - remaining:  letters, digits, underscores
//   - length 1..63 bytes (NAMEDATALEN-1 for default builds)
//
// Quoted identifiers can contain richer characters, but every CLI/
// config surface we accept names through interpolates them into
// unquoted positions (replication-protocol commands, slot files,
// audit events), so the unquoted rule is the right gate.
//
// Slot names are folded to lowercase by PG; this check accepts
// uppercase so the caller can fold once and reuse, but a strict
// caller should additionally require strings.ToLower(s) == s.
func ValidIdentifier(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	b := s[0]
	if !(b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')) {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
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
