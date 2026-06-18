// env_var_yaml_test.go — coverage for the PG_HARDSTORAGE_CONFIG
// inline-YAML env var (issue #87).
//
// The docker-compose evaluation stack relies on this env var to
// drive a single-container deployment without bind-mounting a
// file.  Pre-fix the env var was silently ignored and the agent
// crash-looped with `config.no_deployments` even though the
// content was visible in `docker inspect`.

package config_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
)

// Issue #87's exact reproducer: the env var carries a YAML body
// declaring one deployment.  No on-disk config file exists.  The
// loader must surface the deployment so the agent doesn't error
// with `config.no_deployments`.
func TestLoad_EnvVarYAML_PopulatesDeployments(t *testing.T) {
	tmp := t.TempDir()
	p := pathsForTempDir(t, tmp)

	t.Setenv("PG_HARDSTORAGE_CONFIG", `
deployments:
  demo:
    pg_connection: postgres://postgres:demo@pg:5432/demo
    repo: s3://pg-hardstorage-demo/
    schedule:
      backup: { every: "6h" }
      rotate: { daily_at: "04:00" }
`)

	res, err := config.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := len(res.Config.Deployments); got != 1 {
		t.Fatalf("deployments = %d, want 1 (issue #87 regression: env-var YAML silently ignored)", got)
	}
	dep, ok := res.Config.Deployments["demo"]
	if !ok {
		t.Fatal("deployments.demo missing")
	}
	if !strings.HasPrefix(dep.PGConnection, "postgres://") {
		t.Errorf("PGConnection = %q, want a libpq URI", dep.PGConnection)
	}
	if dep.Repo != "s3://pg-hardstorage-demo/" {
		t.Errorf("Repo = %q", dep.Repo)
	}
	if dep.Schedule.Backup.Every != "6h" {
		t.Errorf("Schedule.Backup.Every = %q", dep.Schedule.Backup.Every)
	}
	if dep.Schedule.Rotate.DailyAt != "04:00" {
		t.Errorf("Schedule.Rotate.DailyAt = %q", dep.Schedule.Rotate.DailyAt)
	}

	// SourceFiles must record the env source so doctor can show
	// the operator where the config came from.
	var sawEnv bool
	for _, sf := range res.SourceFiles {
		if sf.Kind == "env" && sf.ReadOK {
			sawEnv = true
			if !strings.Contains(sf.Path, "PG_HARDSTORAGE_CONFIG") {
				t.Errorf("env source label = %q, want one that names the env var", sf.Path)
			}
		}
	}
	if !sawEnv {
		t.Error("SourceFiles missing the env-var entry — doctor would hide the source")
	}
}

// File-on-disk must beat the env-var.  Operators may set
// PG_HARDSTORAGE_CONFIG once as a baseline (compose, K8s) and then
// override per-host via a bind-mounted pg_hardstorage.yaml.
func TestLoad_EnvVarYAML_OverriddenByMainFile(t *testing.T) {
	tmp := t.TempDir()
	p := pathsForTempDir(t, tmp)

	t.Setenv("PG_HARDSTORAGE_CONFIG", `
deployments:
  fromEnv:
    pg_connection: postgres://env@h/db
    repo: s3://from-env/
`)
	writeFile(t, filepath.Join(tmp, "pg_hardstorage.yaml"), `
deployments:
  fromFile:
    pg_connection: postgres://file@h/db
    repo: s3://from-file/
`)

	res, err := config.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// mergeConfig replaces the Deployments map wholesale on a
	// non-empty later source — that's what gives file>env
	// precedence.  Pin the resulting state.
	if _, ok := res.Config.Deployments["fromFile"]; !ok {
		t.Error("file deployment missing — file should win over env")
	}
	// fromEnv may or may not survive the merge depending on
	// mergeConfig's policy; the contract that matters is that the
	// file's deployment is visible.
}

// Unset / empty / whitespace-only is a no-op: the loader must not
// surface a spurious env-source entry.
func TestLoad_EnvVarYAML_EmptyIsNoOp(t *testing.T) {
	cases := map[string]string{
		"unset":      "",
		"whitespace": "   \n\t  ",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			tmp := t.TempDir()
			p := pathsForTempDir(t, tmp)
			if body == "" {
				// Setenv("") still SETS the variable to empty;
				// our check uses strings.TrimSpace which handles
				// both.  The point is the empty/whitespace case
				// is treated as "not set" semantically.
			}
			t.Setenv("PG_HARDSTORAGE_CONFIG", body)

			res, err := config.Load(p)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			for _, sf := range res.SourceFiles {
				if sf.Kind == "env" {
					t.Errorf("empty env var should not produce a source entry; got %+v", sf)
				}
			}
		})
	}
}

// Parse error in the env var is loud, not silent.  This is the
// inverse of the issue #87 bug: pre-fix nothing happened, post-fix
// a typo'd YAML must surface as an error the operator can act on.
func TestLoad_EnvVarYAML_ParseErrorIsReturned(t *testing.T) {
	tmp := t.TempDir()
	p := pathsForTempDir(t, tmp)
	t.Setenv("PG_HARDSTORAGE_CONFIG", `
deployments:
  demo:
    pg_connection: [this is not a string
`) // unterminated list

	_, err := config.Load(p)
	if err == nil {
		t.Fatal("expected an error for malformed env-var YAML")
	}
	if !strings.Contains(err.Error(), "PG_HARDSTORAGE_CONFIG") {
		t.Errorf("error %q does not name PG_HARDSTORAGE_CONFIG; operator can't tell where the bad config came from", err.Error())
	}
}

// Unknown keys in the env var are loud, just like in a file.  This
// catches the docker-compose drift the reporter's stack also had —
// `repos:`, `storage:`, `encryption:` at the top level are not
// part of the schema and silently ignoring them would let the
// operator believe the storage / encryption block is configured
// when it isn't.
func TestLoad_EnvVarYAML_UnknownKeysAreRejected(t *testing.T) {
	tmp := t.TempDir()
	p := pathsForTempDir(t, tmp)
	t.Setenv("PG_HARDSTORAGE_CONFIG", `
deployments:
  demo:
    pg_connection: postgres://x@h/db
    repo: s3://r/
repos:
  s3://r/:
    storage: { plugin: s3 }
`)

	_, err := config.Load(p)
	if err == nil {
		t.Fatal("expected an error for unknown top-level key `repos`")
	}
	if !strings.Contains(err.Error(), "repos") && !strings.Contains(err.Error(), "field repos") {
		t.Errorf("error %q does not name the unknown field; operator can't fix it", err.Error())
	}
}
