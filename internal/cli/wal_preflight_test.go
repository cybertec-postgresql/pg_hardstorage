package cli

import "testing"

func TestInferRoleFromDSN(t *testing.T) {
	cases := []struct {
		name string
		dsn  string
		want string
	}{
		{"empty", "", ""},
		{"uri-with-user", "postgres://alice@db.example.com/postgres", "alice"},
		{"uri-postgresql-scheme", "postgresql://bob@db/postgres", "bob"},
		{"uri-no-user", "postgres://db.example.com/postgres", ""},
		{"keyword-form", "host=db port=5432 user=carol dbname=postgres", "carol"},
		{"keyword-form-only-user", "user=dave", "dave"},
		{"keyword-form-no-user", "host=db port=5432 dbname=postgres", ""},
		{"uri-bad-parse-returns-empty", "postgres://[invalid:url@", ""},
		{"keyword-form-leading-whitespace", "  user=eve  host=db  ", "eve"},
	}
	for _, c := range cases {
		got := inferRoleFromDSN(c.dsn)
		if got != c.want {
			t.Errorf("%s: inferRoleFromDSN(%q) = %q, want %q", c.name, c.dsn, got, c.want)
		}
	}
}
