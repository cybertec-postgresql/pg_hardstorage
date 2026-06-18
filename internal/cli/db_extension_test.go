package cli_test

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// TestDbInstallExtension_PrintSQL: --print-sql succeeds without
// a PG connection and produces SQL containing the schema +
// views the operator expects.
func TestDbInstallExtension_PrintSQL(t *testing.T) {
	stdout, _, exit := runCLI(t, "db", "install-extension", "--print-sql")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit=%d", exit)
	}
	for _, want := range []string{
		"CREATE TABLE pg_hardstorage.backups_state",
		"CREATE OR REPLACE VIEW pg_hardstorage.backups",
		"CREATE OR REPLACE VIEW pg_hardstorage.health",
		"CREATE OR REPLACE VIEW pg_hardstorage.rpo",
		"pg_hardstorage_writer",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q", want)
		}
	}
}

// TestDbInstallExtension_DryRun: --dry-run reports what would
// happen without a PG connection and without applying.
func TestDbInstallExtension_DryRun(t *testing.T) {
	stdout, _, exit := runCLI(t, "db", "install-extension", "--dry-run", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit=%d", exit)
	}
	for _, want := range []string{
		`"dry_run": true`,
		`"schema": "pg_hardstorage"`,
		`"version": "1.0"`,
		`"views"`,
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("dry-run JSON missing %q\n%s", want, stdout)
		}
	}
}

// TestDbInstallExtension_RequiresConnection: without --pg-connection
// (and no --dry-run / --print-sql), the command refuses with
// usage.missing_flag.
func TestDbInstallExtension_RequiresConnection(t *testing.T) {
	_, stderr, exit := runCLI(t, "db", "install-extension", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("expected ExitMisuse, got %d", exit)
	}
	if !strings.Contains(stderr, "usage.missing_flag") {
		t.Errorf("expected usage.missing_flag; stderr=%s", stderr)
	}
}

// TestDbUninstallExtension_RequiresDropData: without --drop-data
// the command refuses.
func TestDbUninstallExtension_RequiresDropData(t *testing.T) {
	_, stderr, exit := runCLI(t, "db", "uninstall-extension",
		"--pg-connection", "postgres://x@y/z",
		"-o", "json",
	)
	if exit != int(output.ExitMisuse) {
		t.Errorf("expected ExitMisuse, got %d", exit)
	}
	if !strings.Contains(stderr, "usage.confirmation_required") {
		t.Errorf("expected usage.confirmation_required; stderr=%s", stderr)
	}
}
