// precedence_truthfulness_test.go — pins the documented
// precedence ladder for config sources.
//
// Issue #87 (the PG_HARDSTORAGE_CONFIG env-var fix) surfaced
// the deeper question: when multiple config sources disagree,
// which one wins?  The docstring on Load says:
//
//	env-var lowest precedence → main file overrides env →
//	conf.d/*.yaml overrides main file → lexicographic name
//	order within conf.d (later wins, per /etc/sysctl.d
//	convention)
//
// Operators script around this ("set FOO in conf.d/99-prod.yaml
// to override the base config" is documented as a pattern).
// A silent change to the order would break those scripts in
// hard-to-debug ways.
//
// This test exercises a representative field with non-trivial
// merge semantics (a deployment's repo URL) through every
// combination of env / main / conf.d sources and asserts the
// winner matches the documented order.
package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
)

// setupPrecedenceWorld returns a fresh Paths rooted in a temp
// dir with the right XDG env vars + an empty conf.d.  Each
// test plants the sources it cares about and runs Load.
func setupPrecedenceWorld(t *testing.T) *paths.Paths {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))

	// Make sure no env-var config leaks from the test runner.
	t.Setenv("PG_HARDSTORAGE_CONFIG", "")

	p, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{p.Config.Value, p.ConfigDropIn.Value} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

// planted writes a YAML body to path, fatal on error.
func planted(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestPrecedence_MainOverridesEnv: main config file beats the
// env-var inline YAML.  Docstring: env is "lowest precedence".
func TestPrecedence_MainOverridesEnv(t *testing.T) {
	p := setupPrecedenceWorld(t)
	t.Setenv("PG_HARDSTORAGE_CONFIG", `deployments:
  db1:
    repo: "from-env"`)
	planted(t, filepath.Join(p.Config.Value, "pg_hardstorage.yaml"),
		`deployments:
  db1:
    repo: "from-main"`)
	res, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Config.Deployments["db1"].Repo; got != "from-main" {
		t.Errorf("repo = %q; want from-main (main file must override env)", got)
	}
}

// TestPrecedence_DropInOverridesMain: a conf.d/*.yaml beats the
// main config file.  Operator pattern: "ship the base config in
// pg_hardstorage.yaml; override per-host knobs in conf.d/99-
// prod.yaml so it survives an upgrade that ships a fresh main
// file."  This is the load-bearing precedence for the operator's
// playbook.
func TestPrecedence_DropInOverridesMain(t *testing.T) {
	p := setupPrecedenceWorld(t)
	planted(t, filepath.Join(p.Config.Value, "pg_hardstorage.yaml"),
		`deployments:
  db1:
    repo: "from-main"`)
	planted(t, filepath.Join(p.ConfigDropIn.Value, "99-prod.yaml"),
		`deployments:
  db1:
    repo: "from-drop-in"`)
	res, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Config.Deployments["db1"].Repo; got != "from-drop-in" {
		t.Errorf("repo = %q; want from-drop-in (conf.d must override main)", got)
	}
}

// TestPrecedence_LexicographicWithinDropIn: a later-sorting
// file in conf.d/ beats an earlier one.  Mirrors /etc/sysctl.d
// (the "99-" prefix convention operators expect).
func TestPrecedence_LexicographicWithinDropIn(t *testing.T) {
	p := setupPrecedenceWorld(t)
	planted(t, filepath.Join(p.ConfigDropIn.Value, "10-base.yaml"),
		`deployments:
  db1:
    repo: "from-10"`)
	planted(t, filepath.Join(p.ConfigDropIn.Value, "90-override.yaml"),
		`deployments:
  db1:
    repo: "from-90"`)
	res, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Config.Deployments["db1"].Repo; got != "from-90" {
		t.Errorf("repo = %q; want from-90 (lexicographic late wins)", got)
	}
}

// TestPrecedence_FullStack: env + main + two drop-ins; the
// latest drop-in must win across the whole stack.
func TestPrecedence_FullStack(t *testing.T) {
	p := setupPrecedenceWorld(t)
	t.Setenv("PG_HARDSTORAGE_CONFIG", `deployments:
  db1:
    repo: "from-env"`)
	planted(t, filepath.Join(p.Config.Value, "pg_hardstorage.yaml"),
		`deployments:
  db1:
    repo: "from-main"`)
	planted(t, filepath.Join(p.ConfigDropIn.Value, "10-base.yaml"),
		`deployments:
  db1:
    repo: "from-10"`)
	planted(t, filepath.Join(p.ConfigDropIn.Value, "99-prod.yaml"),
		`deployments:
  db1:
    repo: "from-99"`)
	res, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Config.Deployments["db1"].Repo; got != "from-99" {
		t.Errorf("full-stack winner = %q; want from-99 (env→main→10→99 ladder)", got)
	}
}

// TestPrecedence_EnvAloneIsSufficient: when no files exist, the
// env-var alone populates the config.  Required for the docker-
// compose evaluation contract documented in issue #87.
func TestPrecedence_EnvAloneIsSufficient(t *testing.T) {
	p := setupPrecedenceWorld(t)
	t.Setenv("PG_HARDSTORAGE_CONFIG", `deployments:
  db1:
    repo: "from-env-only"`)
	res, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Config.Deployments["db1"].Repo; got != "from-env-only" {
		t.Errorf("repo = %q; want from-env-only (env-only path)", got)
	}
}
