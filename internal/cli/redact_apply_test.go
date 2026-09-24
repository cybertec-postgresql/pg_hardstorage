package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

func writeRedactRules(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "rules.yaml")
	if err := os.WriteFile(p, []byte("tables:\n  public.users:\n    columns:\n      email: hash_keep_domain\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// Regression (LOW): `redact apply --print-sql -o json` wrote raw SQL
// and no Result, so a JSON consumer got unparseable output. In JSON
// mode the SQL travels inside the Result body.
func TestRedactApply_PrintSQLJSONIsAResult(t *testing.T) {
	_ = newReadWorld(t)
	stdout, stderr, exit := runCLI(t, "redact", "apply", "--rules", writeRedactRules(t),
		"--salt-hex", "deadbeef00010203", "--print-sql", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit=%d\n%s\n%s", exit, stdout, stderr)
	}
	var doc struct {
		Result struct {
			SQL string `json:"sql"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("stdout is not a JSON Result: %v\n%s", err, stdout)
	}
	if !strings.Contains(doc.Result.SQL, "UPDATE \"public\".\"users\"") && !strings.Contains(doc.Result.SQL, "UPDATE public.users") {
		t.Errorf("result.sql lacks the UPDATE: %q", doc.Result.SQL)
	}

	// Text mode keeps the raw, pipeable SQL.
	stdout, _, _ = runCLI(t, "redact", "apply", "--rules", writeRedactRules(t),
		"--salt-hex", "deadbeef00010203", "--print-sql", "-o", "text")
	if !strings.HasPrefix(stdout, "BEGIN;") {
		t.Errorf("text --print-sql should stay raw SQL:\n%s", stdout)
	}
}

// Regression (LOW): psql received the full --pg-connection DSN, with
// its password, on argv — readable by every local user via ps. The
// password must travel in the environment (PGPASSWORD) instead.
func TestRedactApply_PasswordNotOnPsqlArgv(t *testing.T) {
	_ = newReadWorld(t)
	bin := t.TempDir()
	logf := filepath.Join(bin, "psql.log")
	stub := "#!/bin/sh\nprintf 'ARGV %s\\n' \"$*\" >> '" + logf + "'\nprintf 'PGPASSWORD %s\\n' \"$PGPASSWORD\" >> '" + logf + "'\ncat >/dev/null\n"
	if err := os.WriteFile(filepath.Join(bin, "psql"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	for _, dsn := range []string{
		"postgres://alice:s3cr%40t@db.example:5433/app?sslmode=require",
		"host=db.example port=5433 user=alice password='s3cr@t' dbname=app sslmode=require",
	} {
		_ = os.Remove(logf)
		_, stderr, exit := runCLI(t, "redact", "apply", "--rules", writeRedactRules(t),
			"--pg-connection", dsn, "-o", "json")
		if exit != int(output.ExitOK) {
			t.Fatalf("exit=%d\n%s", exit, stderr)
		}
		raw, err := os.ReadFile(logf)
		if err != nil {
			t.Fatal(err)
		}
		log := string(raw)
		argv := strings.SplitN(log, "\n", 2)[0]
		if strings.Contains(argv, "s3cr") {
			t.Errorf("password on psql argv for %q:\n%s", dsn, argv)
		}
		if !strings.Contains(argv, "db.example") || !strings.Contains(argv, "app") {
			t.Errorf("connection target lost from argv for %q:\n%s", dsn, argv)
		}
		if !strings.Contains(log, "PGPASSWORD s3cr@t") {
			t.Errorf("password not passed via PGPASSWORD for %q:\n%s", dsn, log)
		}
	}
}
