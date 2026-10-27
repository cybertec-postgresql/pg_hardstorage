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
