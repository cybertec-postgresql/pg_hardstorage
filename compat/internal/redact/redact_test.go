package redact

import (
	"strings"
	"testing"
)

func TestValue(t *testing.T) {
	for _, tc := range []struct{ k, v, leak string }{
		{"AWS_SECRET_ACCESS_KEY", "abc123", "abc123"},
		{"repo1-s3-key-secret", "abc123", "abc123"},
		{"repo1-cipher-pass", "abc123", "abc123"},
		{"PGPASSWORD", "abc123", "abc123"},
		{"conninfo", "host=h password=abc123 user=u", "abc123"},
		{"conninfo", "host=h password='a b c' user=u", "a b c"},
		{"repo", "s3://user:abc123@host/bucket", "abc123"},
	} {
		got := Value(tc.k, tc.v)
		if strings.Contains(got, tc.leak) {
			t.Errorf("Value(%q, %q) = %q leaks the secret", tc.k, tc.v, got)
		}
	}
	if got := Value("log-level-console", "info"); got != "info" {
		t.Errorf("harmless value altered: %q", got)
	}
	if got := Value("conninfo", "host=h user=u"); got != "host=h user=u" {
		t.Errorf("password-less conninfo altered: %q", got)
	}
}
