package cli

import (
	"strings"
	"testing"
)

// TestRedactDSN_LibpqGrammar pins M55: redaction follows libpq's
// keyword/value grammar (spaces around '=', quoted values with escaped
// quotes, backslash escapes in unquoted values) and the URI form, so no
// valid spelling of a password survives into `deployment list`.
func TestRedactDSN_LibpqGrammar(t *testing.T) {
	cases := []struct {
		in     string
		secret string
		keep   []string
	}{
		{"host=h password=secret dbname=x", "secret", []string{"host=h", "dbname=x"}},
		{"host=h password = secret dbname=x", "secret", []string{"host=h", "dbname=x"}},
		{"host=h password= secret dbname=x", "secret", []string{"dbname=x"}},
		{"host=h password =secret", "secret", []string{"host=h"}},
		{`password='it\'s a secret' user=u`, "secret", []string{"user=u"}},
		{`password='back\\slash' user=u`, "slash", []string{"user=u"}},
		{`password=se\ cret user=u`, "cret", []string{"user=u"}},
		{"sslpassword=keypass sslmode=verify-full", "keypass", []string{"sslmode=verify-full"}},
		{"host=h\tpassword\t=\tsecret\tuser=u", "secret", []string{"user=u"}},
		{"postgres://u:secret@h/db", "secret", []string{"u:", "@h/db"}},
		{"postgresql://u:p%40ss@h1:5432,h2:5433/db?sslmode=require", "p%40ss", []string{"h1:5432,h2:5433/db", "sslmode=require"}},
		{"postgres://u@h/db?sslmode=require&password=secret", "secret", []string{"sslmode=require"}},
		{"postgres://u@h/db?PASSWORD=secret", "secret", nil},
		{"postgres://u:pa@ss@h/db", "pa@ss", []string{"@h/db"}},
	}
	for _, c := range cases {
		got := redactDSN(c.in)
		if strings.Contains(got, c.secret) {
			t.Errorf("redactDSN(%q) = %q — still contains %q", c.in, got, c.secret)
		}
		if !strings.Contains(got, "****") {
			t.Errorf("redactDSN(%q) = %q — no redaction marker", c.in, got)
		}
		for _, k := range c.keep {
			if !strings.Contains(got, k) {
				t.Errorf("redactDSN(%q) = %q — lost %q", c.in, got, k)
			}
		}
	}
	// Nothing to hide: returned unchanged.
	for _, clean := range []string{"", "host=h user=u", "postgres://u@h/db?sslmode=disable", "host=/var/run/postgresql"} {
		if got := redactDSN(clean); got != clean {
			t.Errorf("redactDSN(%q) = %q, want unchanged", clean, got)
		}
	}
}
