package redact_test

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore/redact"
)

func TestParseRules_Basic(t *testing.T) {
	body := []byte(`
schema: pg_hardstorage.redact.v1
tables:
  public.users:
    columns:
      email: hash_keep_domain
      ssn: nullify
      name: hash_to_uuid
`)
	r, err := redact.ParseRules(body)
	if err != nil {
		t.Fatalf("ParseRules: %v", err)
	}
	if r.Tables["public.users"].Columns["email"] != "hash_keep_domain" {
		t.Errorf("strategy lost: %v", r.Tables)
	}
}

func TestParseRules_RejectsUnknownStrategy(t *testing.T) {
	body := []byte(`
tables:
  public.users:
    columns:
      email: very_clever_strategy
`)
	if _, err := redact.ParseRules(body); err == nil {
		t.Error("expected unknown-strategy refusal")
	}
}

func TestParseRules_RejectsBadTableName(t *testing.T) {
	body := []byte(`
tables:
  notqualified:
    columns:
      x: nullify
`)
	if _, err := redact.ParseRules(body); err == nil {
		t.Error("expected bad-table refusal")
	}
}

func TestParseRules_RejectsBadColumnName(t *testing.T) {
	body := []byte(`
tables:
  public.users:
    columns:
      "bad name with spaces": nullify
`)
	if _, err := redact.ParseRules(body); err == nil {
		t.Error("expected bad-column refusal")
	}
}

func TestRedactValue_Strategies(t *testing.T) {
	salt := []byte("super-secret-salt")
	cases := []struct {
		strategy redact.Strategy
		input    string
		want     string
	}{
		{"nullify", "anything", ""},
		{"replace_with_xxx", "secret", "xxxxxx"},
		{"replace_with_xxx", "", ""},
		{"constant:[redacted]", "anything", "[redacted]"},
		{"hash_keep_domain", "alice@acme.com", ""},
		{"regex:\\d+:#", "ssn=123-45-6789", "ssn=#-#-#"},
	}
	for _, tc := range cases {
		got := redact.RedactValue(tc.strategy, salt, tc.input)
		switch tc.strategy {
		case "hash_keep_domain":
			if !strings.HasSuffix(got, "@acme.com") {
				t.Errorf("hash_keep_domain dropped domain: %q", got)
			}
			if got == tc.input {
				t.Errorf("hash_keep_domain didn't change input: %q", got)
			}
		default:
			if got != tc.want {
				t.Errorf("RedactValue(%q,%q) = %q, want %q", tc.strategy, tc.input, got, tc.want)
			}
		}
	}
}

func TestRedactValue_HashIsDeterministic(t *testing.T) {
	salt := []byte("salt")
	a := redact.RedactValue("hash_to_uuid", salt, "alice")
	b := redact.RedactValue("hash_to_uuid", salt, "alice")
	if a != b {
		t.Errorf("hash should be deterministic: %q vs %q", a, b)
	}
	c := redact.RedactValue("hash_to_uuid", []byte("different-salt"), "alice")
	if a == c {
		t.Errorf("different salt should produce different hash; got %q", a)
	}
}

func TestPlan_SQLBuilds(t *testing.T) {
	rules, err := redact.ParseRules([]byte(`
tables:
  public.users:
    columns:
      email: hash_keep_domain
      ssn: nullify
`))
	if err != nil {
		t.Fatal(err)
	}
	p, err := redact.NewPlan(rules)
	if err != nil {
		t.Fatal(err)
	}
	stmts := p.SQL()
	if len(stmts) != 1 {
		t.Fatalf("expected 1 stmt; got %d", len(stmts))
	}
	stmt := stmts[0]
	if !strings.HasPrefix(stmt.Stmt, `UPDATE "public"."users" SET `) {
		t.Errorf("UPDATE prefix wrong: %q", stmt.Stmt)
	}
	// columns alpha-sorted: email, ssn
	if !strings.Contains(stmt.Stmt, `"email" =`) || !strings.Contains(stmt.Stmt, `"ssn" = NULL`) {
		t.Errorf("missing column expressions: %q", stmt.Stmt)
	}
}

func TestPlan_SaltOverride(t *testing.T) {
	rules, _ := redact.ParseRules([]byte(`tables:
  public.x:
    columns:
      a: nullify`))
	p, _ := redact.NewPlan(rules)
	custom := []byte("operator-supplied-salt")
	if err := p.SetSalt(custom); err != nil {
		t.Fatal(err)
	}
	if p.SaltHex() == "" {
		t.Error("SaltHex should be non-empty")
	}
	// Refuse short salts.
	if err := p.SetSalt([]byte("tiny")); err == nil {
		t.Error("SetSalt should refuse short salts")
	}
}

func TestParseRules_RejectsBadRegex(t *testing.T) {
	body := []byte(`
tables:
  public.users:
    columns:
      x: "regex:[unterminated:replacement"
`)
	if _, err := redact.ParseRules(body); err == nil {
		t.Error("expected bad-regex refusal")
	}
}

func TestParseRules_EmptyTablesRefused(t *testing.T) {
	body := []byte(`
tables: {}
`)
	if _, err := redact.ParseRules(body); err == nil {
		t.Error("expected empty-rules refusal")
	}
}
