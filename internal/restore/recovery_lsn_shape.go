//go:build !mutation_lsn_shape_loose

// recovery_lsn_shape.go — LooksLikeLSN strict-shape check (real impl).
//
// Split into its own file so the mutation-testing harness can swap in
// a deliberately-broken variant under the mutation_lsn_shape_loose
// build tag.  See internal/testkit/mutation/registry.go.
package restore

import "strings"

// LooksLikeLSN reports whether s has the PostgreSQL LSN textual shape
// "<hex>/<hex>" with at least one hex digit on each side, exactly one
// slash separator, and no other characters. Intentionally stricter
// than pglogrepl.ParseLSN, which silently truncates trailing garbage;
// pair this with ParseLSN when numeric value matters. Safe to call
// from CLI, server route, and agent layers.
//
// Mutation-testing note: a deliberately-broken variant lives in
// recovery_lsn_shape_mutation_loose.go (selected by
// `go test -tags=mutation_lsn_shape_loose`) that drops the "exactly
// one slash" check and re-introduces the silent-multi-slash bug the
// property test caught.  The test suite must fail under the tag.
func LooksLikeLSN(s string) bool {
	// Exactly one '/' — earlier revisions only checked for the first
	// slash position and a generative test (rapid) caught "0//0"
	// passing through.  Multiple slashes can never be a valid LSN
	// shape and must be rejected at the boundary.
	i := strings.IndexByte(s, '/')
	if i <= 0 || i == len(s)-1 {
		return false
	}
	if strings.IndexByte(s[i+1:], '/') >= 0 {
		return false
	}
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
