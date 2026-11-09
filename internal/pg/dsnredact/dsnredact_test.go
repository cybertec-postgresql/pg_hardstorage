package dsnredact

import (
	"strings"
	"testing"
)

func TestRedactNeverLeaks(t *testing.T) {
	const pw = "s3cr3t!pw"
	for _, dsn := range []string{
		"postgres://u:" + pw + "@h/db",
		"postgres://u:p@ss" + "@h/db", // unencoded @ in password
		"postgresql://u@h/db?password=" + pw,
		"postgres://u@h/db?sslpassword=" + pw,
		"host=h user=u password=" + pw + " dbname=d",
		"host=h password = " + pw,
		"host=h password='" + pw + "'",
		"host=h password='it\\'s " + pw + "'",
		"host=h sslpassword=" + pw,
		"password=" + pw + " garbage",
		"host=h password=" + pw + " =bad",
	} {
		out := Redact(dsn)
		for _, secret := range []string{pw, "p@ss", "ss@h"} {
			if strings.Contains(dsn, secret) && strings.Contains(out, secret) {
				t.Errorf("Redact(%q) = %q leaks %q", dsn, out, secret)
			}
		}
	}
}

func FuzzRedact(f *testing.F) {
	f.Add("host=h password=abc123 dbname=d")
	f.Add("postgres://u:abc123@h/d?password=abc123")
	f.Fuzz(func(t *testing.T, s string) {
		out := Redact(s)
		_ = out
	})
}

// A DSN the tokenizer cannot parse is redacted from the first secret
// keyword onward. Bytes that are not valid UTF-8, or runes whose lower
// case has a different length, must neither crash that search (it used
// to slice the original at an offset into its lower-cased copy) nor
// shift the cut so the secret survives.
func TestRedactMalformedNonASCIITail(t *testing.T) {
	const pw = "s3cr3t!pw"
	for _, c := range []struct{ dsn, want string }{
		{"\xd3\xe8\xc30\x9c\x9dpAssword", "\xd3\xe8\xc30\x9c\x9d" + redactedValue},
		{"\xff\xfe\xfd PASSWORD " + pw, "\xff\xfe\xfd " + redactedValue},
		{"İİİİ sslPassword " + pw, "İİİİ ssl" + redactedValue}, // U+0130 lower-cases to 3 bytes
		{"KKKK Password " + pw, "KKKK " + redactedValue},       // Kelvin sign lower-cases to 1 byte
		{"host=h \xc3 nonsense", "host=h \xc3 nonsense"},       // no keyword: untouched
	} {
		got := Redact(c.dsn)
		if got != c.want {
			t.Errorf("Redact(%q) = %q, want %q", c.dsn, got, c.want)
		}
		if strings.Contains(got, pw) {
			t.Errorf("Redact(%q) = %q leaks the password", c.dsn, got)
		}
	}
}
