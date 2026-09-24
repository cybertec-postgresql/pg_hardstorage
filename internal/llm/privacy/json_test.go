package privacy_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/privacy"
)

// Tool results reach the redactor as JSON.  The credential patterns
// were written for key=value text: `"password":"x"` has a quote between
// the key and the colon, so it slipped through un-redacted.
func TestRedact_JSONShapedSecrets(t *testing.T) {
	cases := []struct{ in, secret string }{
		{`{"password":"hunter2"}`, "hunter2"},
		{`{"password": "hunter2"}`, "hunter2"},
		{`{"api_key":"sk-abc123"}`, "sk-abc123"},
		{`{"db_password":"hunter2"}`, "hunter2"},
		{`{"client_secret":"s3cr3t"}`, "s3cr3t"},
		{`{"aws_secret_access_key":"wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"}`, "wJalrXUtnFEMI"},
		{`{"Authorization":"Bearer eyJhbGciOi.payload.sig"}`, "eyJhbGciOi"},
		{`{"password":"with \"escaped\" quote"}`, "escaped"},
	}
	for _, m := range []privacy.Mode{privacy.ModeOpen, privacy.ModeStandard} {
		for _, c := range cases {
			got := privacy.Redact(m, c.in)
			if strings.Contains(got, c.secret) {
				t.Errorf("mode %s: %s leaked %q: %s", m, c.in, c.secret, got)
			}
			if !json.Valid([]byte(got)) {
				t.Errorf("mode %s: redaction broke JSON: %s -> %s", m, c.in, got)
			}
		}
	}
}

// The old value class `[^<\s]+` ran to the next whitespace — in compact
// JSON that is the rest of the document, so every field after a
// password was destroyed (and the output was no longer JSON).
func TestRedact_CompactJSONNotOverRedacted(t *testing.T) {
	in := `{"password":"hunter2","deployment":"db1","status":"ok","token_count":3}`
	got := privacy.Redact(privacy.ModeOpen, in)
	if strings.Contains(got, "hunter2") {
		t.Fatalf("password leaked: %s", got)
	}
	for _, keep := range []string{`"deployment":"db1"`, `"status":"ok"`, `"token_count":3`} {
		if !strings.Contains(got, keep) {
			t.Errorf("over-redaction: %s missing from %s", keep, got)
		}
	}
	if !json.Valid([]byte(got)) {
		t.Errorf("redaction broke JSON: %s", got)
	}
	// Bearer in compact JSON likewise.
	in = `{"authorization":"Bearer abc","deployment":"db1"}`
	got = privacy.Redact(privacy.ModeOpen, in)
	if strings.Contains(got, "abc") || !strings.Contains(got, `"deployment":"db1"`) {
		t.Errorf("bearer redaction wrong: %s", got)
	}
	// Query-string form stops at '&'.
	got = privacy.Redact(privacy.ModeOpen, "https://x/cb?password=hunter2&region=eu")
	if strings.Contains(got, "hunter2") || !strings.Contains(got, "region=eu") {
		t.Errorf("query redaction wrong: %s", got)
	}
}

// A password containing '@' is legal in a connection URL; the old
// pattern stopped at the first '@' and leaked the rest.
func TestRedact_PostgresURLPasswordWithAt(t *testing.T) {
	in := "dsn postgres://backup:p@ss@db1.example.com:5432/postgres"
	got := privacy.Redact(privacy.ModeOpen, in)
	if strings.Contains(got, "ss@") || strings.Contains(got, "p@ss") {
		t.Errorf("password tail leaked: %s", got)
	}
	if !strings.Contains(got, "db1.example.com:5432/postgres") {
		t.Errorf("host should survive in open mode: %s", got)
	}
	// Inside compact JSON the match must not run into the next field.
	in = `{"dsn":"postgres://u:pw@h/db","contact":"a@b"}`
	got = privacy.Redact(privacy.ModeOpen, in)
	if strings.Contains(got, "pw@") || !strings.Contains(got, `"contact":"a@b"`) {
		t.Errorf("json dsn redaction wrong: %s", got)
	}
}

func TestRedact_JSONIdempotent(t *testing.T) {
	in := `{"password":"hunter2","authorization":"Bearer x","dsn":"postgres://u:p@ss@h/db"}`
	for _, m := range []privacy.Mode{privacy.ModeOpen, privacy.ModeStandard, privacy.ModeStrict} {
		once := privacy.Redact(m, in)
		if twice := privacy.Redact(m, once); once != twice {
			t.Errorf("mode %s not idempotent:\n once:  %s\n twice: %s", m, once, twice)
		}
	}
}
