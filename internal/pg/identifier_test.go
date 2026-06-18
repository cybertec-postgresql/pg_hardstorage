// identifier_test.go — guard the PG-identifier shape rules used by
// every replication slot / publication name validation site.
package pg_test

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
)

func TestValidIdentifier(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		// happy path
		{"slot_a", true},
		{"_underscore", true},
		{"PgUpper", true},
		{"a", true},
		{"a1", true},
		{strings.Repeat("a", 63), true},
		// rejected
		{"", false},
		{"1leading_digit", false},
		{"has space", false},
		{"semi;colon", false},
		{"quote'in", false},
		{"dquote\"in", false},
		{"slash/in", false},
		{"dash-in", false},
		{"dot.in", false},
		{"newline\nin", false},
		{"null\x00byte", false},
		{strings.Repeat("a", 64), false},
	}
	for _, c := range cases {
		got := pg.ValidIdentifier(c.in)
		if got != c.want {
			t.Errorf("ValidIdentifier(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
