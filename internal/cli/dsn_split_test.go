package cli

import "testing"

// TestSplitDSNPassword: the password leaves the DSN (decoded, ready for
// PGPASSWORD) and everything else — sslpassword included — stays, so the
// stripped DSN still connects.
func TestSplitDSNPassword(t *testing.T) {
	cases := []struct{ in, stripped, pw string }{
		{`host=h password='it\'s' user=u`, "host=h  user=u", "it's"},
		{"host=h password = s3 sslpassword=k", "host=h  sslpassword=k", "s3"},
		{"postgres://u:p%40ss@h/db?sslmode=require", "postgres://u@h/db?sslmode=require", "p@ss"},
		{"postgres://u@h/db?password=a%26b&sslpassword=k", "postgres://u@h/db?sslpassword=k", "a&b"},
		{"postgres://u@h/db?password=x", "postgres://u@h/db", "x"},
	}
	for _, c := range cases {
		got, pw, ok := splitDSNPassword(c.in)
		if !ok || got != c.stripped || pw != c.pw {
			t.Errorf("splitDSNPassword(%q) = (%q, %q, %v), want (%q, %q, true)", c.in, got, pw, ok, c.stripped, c.pw)
		}
	}
	if _, _, ok := splitDSNPassword("host=h user=u"); ok {
		t.Error("no password: ok must be false")
	}
}
