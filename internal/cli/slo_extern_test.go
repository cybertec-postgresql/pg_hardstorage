package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func seedSloConfig(t *testing.T, dir, name string) {
	t.Helper()
	body := `schema: pg_hardstorage.config.v1
deployments:
  ` + name + `:
    pg_connection: postgres://x@h/db
    repo: file:///tmp/x
`
	if err := os.WriteFile(filepath.Join(dir, "pg_hardstorage.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSLO_Set_HappyPath(t *testing.T) {
	dir := configDir(t)
	seedSloConfig(t, dir, "db1")

	out, _, exit := runCmd(t,
		"slo", "set", "db1",
		"--rpo", "1h",
		"--rto", "10m",
		"--output", "json",
	)
	if exit != 0 {
		t.Fatalf("exit = %d, out:\n%s", exit, out)
	}
	for _, want := range []string{
		`"rpo_seconds": 3600`,
		`"rto_seconds": 600`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestSLO_Set_AcceptsDayShorthand(t *testing.T) {
	dir := configDir(t)
	seedSloConfig(t, dir, "db1")
	out, _, exit := runCmd(t,
		"slo", "set", "db1",
		"--rpo", "7d",
		"--output", "json",
	)
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	// 7 days = 604800 seconds.
	if !strings.Contains(out, `"rpo_seconds": 604800`) {
		t.Errorf("7d should parse to 604800 seconds:\n%s", out)
	}
}

func TestSLO_Set_RequiresAtLeastOneFlag(t *testing.T) {
	dir := configDir(t)
	seedSloConfig(t, dir, "db1")
	_, _, exit := runCmd(t,
		"slo", "set", "db1",
		"--output", "json",
	)
	if exit != 2 {
		t.Errorf("set without flags should exit 2; got %d", exit)
	}
}

func TestSLO_Set_RejectsBadDuration(t *testing.T) {
	dir := configDir(t)
	seedSloConfig(t, dir, "db1")
	_, _, exit := runCmd(t,
		"slo", "set", "db1",
		"--rpo", "rugby",
		"--output", "json",
	)
	if exit != 2 {
		t.Errorf("bad duration should exit 2; got %d", exit)
	}
}

func TestSLO_Show_FleetWide(t *testing.T) {
	dir := configDir(t)
	body := `schema: pg_hardstorage.config.v1
deployments:
  db1:
    pg_connection: postgres://x@h/db
    repo: file:///tmp/x
    slo:
      rpo_seconds: 3600
      rto_seconds: 600
  db2:
    pg_connection: postgres://x@h/db
    repo: file:///tmp/x
`
	if err := os.WriteFile(filepath.Join(dir, "pg_hardstorage.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _, exit := runCmd(t, "slo", "show", "--output", "json")
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	for _, want := range []string{
		`"deployment": "db1"`,
		`"rpo_seconds": 3600`,
		`"deployment": "db2"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// writeReadWorldConfig writes a pg_hardstorage.yaml into the
// readWorld's config dir — the same HOME-derived path the CLI's
// loadEditableConfig will read. Bypasses configDir(t) because that
// helper sets PG_HARDSTORAGE_CONFIG_DIR to a fresh tempdir,
// decoupling the YAML from the keyring newReadWorld set up.
func writeReadWorldConfig(t *testing.T, w *readWorld, body string) {
	t.Helper()
	if err := os.MkdirAll(w.configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.configDir, "pg_hardstorage.yaml"),
		[]byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSLO_Report_MetTarget(t *testing.T) {
	w := newReadWorld(t)
	commitVerifiableBackup(t, w, "db1", 0, []byte("x"))
	writeReadWorldConfig(t, w, `schema: pg_hardstorage.config.v1
deployments:
  db1:
    pg_connection: postgres://x@h/db
    repo: `+w.repoURL+`
    slo:
      rpo_seconds: 86400
`)
	out, _, exit := runCLI(t, "slo", "report", "-o", "json")
	if exit != 0 {
		t.Fatalf("exit = %d, out:\n%s", exit, out)
	}
	for _, want := range []string{
		`"deployment": "db1"`,
		`"status": "met"`,
		`"rpo_target_seconds": 86400`,
		`"rpo_actual_seconds":`,
		`"latest_backup":`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestSLO_Report_NoBackups(t *testing.T) {
	w := newReadWorld(t)
	writeReadWorldConfig(t, w, `schema: pg_hardstorage.config.v1
deployments:
  db1:
    pg_connection: postgres://x@h/db
    repo: `+w.repoURL+`
    slo:
      rpo_seconds: 3600
`)
	out, _, exit := runCLI(t, "slo", "report", "-o", "json")
	if exit != 0 {
		t.Fatalf("exit = %d, out:\n%s", exit, out)
	}
	if !strings.Contains(out, `"status": "no_backups"`) {
		t.Errorf("expected no_backups status:\n%s", out)
	}
}

func TestSLO_Clear(t *testing.T) {
	dir := configDir(t)
	seedSloConfig(t, dir, "db1")
	_, _, _ = runCmd(t, "slo", "set", "db1", "--rpo", "1h", "--output", "json")
	out, _, exit := runCmd(t, "slo", "clear", "db1", "--output", "json")
	if exit != 0 {
		t.Fatalf("exit = %d, out:\n%s", exit, out)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "pg_hardstorage.yaml"))
	if strings.Contains(string(body), "rpo_seconds: 3600") {
		t.Errorf("clear should remove rpo_seconds:\n%s", body)
	}
}
