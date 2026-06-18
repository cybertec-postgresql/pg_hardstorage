// Build-tagged integration test: drives `pg_hardstorage init --yes`
// end-to-end against a real PG 17 testcontainer. Asserts the wizard:
//
//   1. probes PG cleanly
//   2. initialises the repo (idempotent across re-runs)
//   3. writes pg_hardstorage.yaml with the deployment block
//   4. takes the first backup and reports it in the Result
//
//go:build integration

package cli_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/testkit"

	"gopkg.in/yaml.v3"
)

func TestIntegration_Init_DayZeroFlow(t *testing.T) {
	srv := testkit.StartPostgres(t)

	cfgDir := t.TempDir()
	t.Setenv("PG_HARDSTORAGE_CONFIG_DIR", cfgDir)

	keyringDir := filepath.Join(t.TempDir(), "keys")
	t.Setenv("PG_HARDSTORAGE_KEYRING_DIR", keyringDir)

	repoURL := "file://" + t.TempDir()

	out, stderr, exit := runCmd(t,
		"init", "--yes",
		"--pg-connection", srv.DSN,
		"--repo", repoURL,
		"--deployment", "db1",
		"--output", "json",
	)
	if exit != 0 {
		t.Fatalf("exit = %d\nstdout: %s\nstderr: %s", exit, out, stderr)
	}

	for _, want := range []string{
		`"deployment": "db1"`,
		`"system_id":`,
		`"repo_url": "` + repoURL + `"`,
		`"first_backup":`,
		`"backup_id":`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\nstdout:\n%s", want, out)
		}
	}

	// Config file should exist with the deployment block populated.
	confPath := filepath.Join(cfgDir, "pg_hardstorage.yaml")
	body, err := os.ReadFile(confPath)
	if err != nil {
		t.Fatalf("config not written: %v", err)
	}
	t.Logf("config:\n%s", body)

	var loaded config.Config
	if err := yaml.Unmarshal(body, &loaded); err != nil {
		t.Fatalf("config not parseable: %v", err)
	}
	dep, ok := loaded.Deployments["db1"]
	if !ok {
		t.Fatal("deployment db1 missing from written config")
	}
	if dep.PGConnection == "" {
		t.Error("pg_connection not written")
	}
	if dep.Repo != repoURL {
		t.Errorf("repo = %q, want %q", dep.Repo, repoURL)
	}
	if dep.Schedule.Backup.Every != "6h" {
		t.Errorf("backup schedule = %+v, want every=6h", dep.Schedule.Backup)
	}
	if dep.Schedule.Rotate.DailyAt != "04:00" {
		t.Errorf("rotate schedule = %+v, want daily_at=04:00", dep.Schedule.Rotate)
	}
}

func TestIntegration_Init_IsIdempotent(t *testing.T) {
	// Run init twice with --skip-backup. The second run must not
	// fail (repo.Init is idempotent, signing keypair survives,
	// config is upserted not crashed).
	srv := testkit.StartPostgres(t)

	cfgDir := t.TempDir()
	t.Setenv("PG_HARDSTORAGE_CONFIG_DIR", cfgDir)
	t.Setenv("PG_HARDSTORAGE_KEYRING_DIR", filepath.Join(t.TempDir(), "keys"))

	repoURL := "file://" + t.TempDir()

	for i := 0; i < 2; i++ {
		out, stderr, exit := runCmd(t,
			"init", "--yes",
			"--pg-connection", srv.DSN,
			"--repo", repoURL,
			"--deployment", "db1",
			"--skip-backup",
			"--output", "json",
		)
		if exit != 0 {
			t.Fatalf("iter %d exit = %d\nstdout: %s\nstderr: %s", i, exit, out, stderr)
		}
	}
}

func TestIntegration_Init_PreservesExistingDeployments(t *testing.T) {
	// Pre-seed the config with a different deployment; init for db1
	// must NOT clobber db2.
	srv := testkit.StartPostgres(t)

	cfgDir := t.TempDir()
	t.Setenv("PG_HARDSTORAGE_CONFIG_DIR", cfgDir)
	t.Setenv("PG_HARDSTORAGE_KEYRING_DIR", filepath.Join(t.TempDir(), "keys"))

	if err := os.WriteFile(filepath.Join(cfgDir, "pg_hardstorage.yaml"), []byte(`
schema: pg_hardstorage.config.v1
deployments:
  db2:
    pg_connection: postgres://x@host/db2
    repo: file:///tmp/pre-existing
    schedule:
      backup: { every: "12h" }
`), 0o644); err != nil {
		t.Fatal(err)
	}

	repoURL := "file://" + t.TempDir()
	_, _, exit := runCmd(t,
		"init", "--yes",
		"--pg-connection", srv.DSN,
		"--repo", repoURL,
		"--deployment", "db1",
		"--skip-backup",
		"--output", "json",
	)
	if exit != 0 {
		t.Fatal("init exit non-zero")
	}

	body, err := os.ReadFile(filepath.Join(cfgDir, "pg_hardstorage.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var loaded config.Config
	if err := yaml.Unmarshal(body, &loaded); err != nil {
		t.Fatal(err)
	}
	if _, ok := loaded.Deployments["db2"]; !ok {
		t.Errorf("init clobbered the pre-existing db2 deployment\nconfig:\n%s", body)
	}
	if _, ok := loaded.Deployments["db1"]; !ok {
		t.Errorf("init didn't add db1\nconfig:\n%s", body)
	}
}

func TestIntegration_Init_RequiresConnectionInYesMode(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("PG_HARDSTORAGE_CONFIG_DIR", cfgDir)
	t.Setenv("PG_HARDSTORAGE_KEYRING_DIR", filepath.Join(t.TempDir(), "keys"))

	_, stderr, exit := runCmd(t,
		"init", "--yes",
		"--repo", "file://"+t.TempDir(),
		"--output", "json",
	)
	if exit != 2 {
		t.Errorf("exit = %d, want 2 (ExitMisuse)", exit)
	}
	if !strings.Contains(stderr, "PostgreSQL connection") && !strings.Contains(stderr, "pg-connection") {
		t.Errorf("error should mention the missing connection; got:\n%s", stderr)
	}
	_ = context.Background
}
