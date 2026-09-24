//go:build integration

package redact_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore/redact"
)

// Regression (LOW): the dry-run preview (RedactValue) must equal what
// the generated SQL writes. The SQL was md5(decode(salt,'hex') ||
// col::text): bytea || text resolves to TEXT concatenation, so
// PostgreSQL hashed the string '\x<salthex>' followed by the value,
// while the preview hashed the raw salt bytes — every preview hash
// differed from the applied one. Checked against a real server,
// including non-ASCII values.
func TestIntegration_RedactSQLMatchesPreview(t *testing.T) {
	bin := func(n string) string {
		p, err := exec.LookPath(n)
		if err != nil {
			t.Skipf("%s not on PATH (export PATH=/usr/lib/postgresql/<v>/bin:$PATH)", n)
		}
		return p
	}
	initdb, pgCtl, psql := bin("initdb"), bin("pg_ctl"), bin("psql")
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "pg")
	if out, err := exec.Command(initdb, "-D", dir, "-U", "postgres", "-A", "trust", "-E", "UTF8", "--no-sync").CombinedOutput(); err != nil {
		t.Fatalf("initdb: %v\n%s", err, out)
	}
	sock, err := os.MkdirTemp("/tmp", "pghs-redact-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sock)
	if out, err := exec.Command(pgCtl, "-D", dir, "-l", filepath.Join(tmp, "log"), "-w",
		"-o", "-p 5432 -c listen_addresses='' -c unix_socket_directories="+sock, "start").CombinedOutput(); err != nil {
		t.Fatalf("pg_ctl start: %v\n%s", err, out)
	}
	defer exec.Command(pgCtl, "-D", dir, "-m", "immediate", "-w", "stop").Run()
	sql := func(q string) string {
		t.Helper()
		out, err := exec.Command(psql, "-X", "-At", "-v", "ON_ERROR_STOP=1", "-h", sock, "-p", "5432",
			"-U", "postgres", "-d", "postgres", "-c", q).CombinedOutput()
		if err != nil {
			t.Fatalf("psql %q: %v\n%s", q, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	rules, err := redact.ParseRules([]byte(`
tables:
  public.users:
    columns:
      id_hash: hash_to_uuid
      email: hash_keep_domain
`))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := redact.NewPlan(rules)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.SetSalt([]byte{0xde, 0xad, 0xbe, 0xef, 0x00, 0x01, 0x02, 0x03}); err != nil {
		t.Fatal(err)
	}

	values := []struct{ id, email string }{
		{"42", "alice@acme.com"},
		{"zoë-ßtraße", "jürgen.schönig@cybertec.at"},
	}
	sql("CREATE TABLE public.users (n int, id_hash text, email text)")
	for i, v := range values {
		sql(fmt.Sprintf("INSERT INTO public.users VALUES (%d, '%s', '%s')", i, v.id, v.email))
	}
	for _, ts := range plan.SQL() {
		sql(ts.Stmt)
	}
	for i, v := range values {
		gotID := sql(fmt.Sprintf("SELECT id_hash FROM public.users WHERE n = %d", i))
		gotEmail := sql(fmt.Sprintf("SELECT email FROM public.users WHERE n = %d", i))
		if want := redact.RedactValue("hash_to_uuid", plan.Salt, v.id); gotID != want {
			t.Errorf("hash_to_uuid(%q): SQL wrote %s, preview says %s", v.id, gotID, want)
		}
		if want := redact.RedactValue("hash_keep_domain", plan.Salt, v.email); gotEmail != want {
			t.Errorf("hash_keep_domain(%q): SQL wrote %s, preview says %s", v.email, gotEmail, want)
		}
	}
}
