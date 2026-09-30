package simple

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
)

func setupEnv(t *testing.T) (*Env, string) {
	t.Helper()
	dir := t.TempDir()
	return &Env{Paths: &paths.Paths{Config: paths.Path{Value: dir}}}, filepath.Join(dir, "pg_hardstorage.yaml")
}

// The config holds pg_connection DSNs, which routinely carry passwords.
// persistDeployment rewrote it 0644 — world-readable — on every setup.
func TestPersistDeployment_ConfigIsNotWorldReadable(t *testing.T) {
	env, cfgPath := setupEnv(t)
	if err := persistDeployment(env, "db1", "postgres://u:s3cret@h/db", "file:///srv/repo"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("config written with mode %o; it holds DSN passwords and must be 0600", perm)
	}
}

// An operator who already tightened the file further (0400) keeps that.
func TestPersistDeployment_PreservesStricterMode(t *testing.T) {
	env, cfgPath := setupEnv(t)
	if err := os.WriteFile(cfgPath, []byte("deployments: {}\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := persistDeployment(env, "db1", "postgres://h/db", "file:///srv/repo"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o400 {
		t.Fatalf("mode = %o, want the operator's stricter 0400 preserved", perm)
	}
}

// Re-running setup for an existing deployment replaced its whole
// mapping node with {pg_connection, repo}, silently dropping kek_ref,
// retention, schedules and anything else the operator had configured.
func TestPersistDeployment_RerunMergesExistingDeployment(t *testing.T) {
	env, cfgPath := setupEnv(t)
	existing := `deployments:
  db1:
    pg_connection: postgres://old/db
    repo: file:///old
    kek_ref: aws-kms://arn:aws:kms:eu-central-1:111122223333:key/abc
    retention:
      keep_daily: 14
  other:
    pg_connection: postgres://o/db
    repo: file:///o
`
	if err := os.WriteFile(cfgPath, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := persistDeployment(env, "db1", "postgres://new/db", "file:///new"); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Deployments map[string]map[string]any `yaml:"deployments"`
	}
	if err := yaml.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	db1 := got.Deployments["db1"]
	if db1["pg_connection"] != "postgres://new/db" || db1["repo"] != "file:///new" {
		t.Errorf("connection/repo not updated: %v", db1)
	}
	if !strings.HasPrefix(stringOf(db1["kek_ref"]), "aws-kms://") {
		t.Errorf("kek_ref dropped on re-run: %v", db1)
	}
	if db1["retention"] == nil {
		t.Errorf("retention dropped on re-run: %v", db1)
	}
	if got.Deployments["other"]["repo"] != "file:///o" {
		t.Errorf("unrelated deployment changed: %v", got.Deployments["other"])
	}
}

func stringOf(v any) string {
	s, _ := v.(string)
	return s
}

// The wizard must accept exactly the names the config loader accepts;
// otherwise it writes a pg_hardstorage.yaml that no longer loads.
func TestValidateDeploymentName_MatchesConfigLoader(t *testing.T) {
	for _, n := range []string{
		"db1", "Prod_main-2", "1db", "_x", "-x", strings.Repeat("a", 64), strings.Repeat("a", 63), "a",
	} {
		wizard := validateDeploymentName(n) == nil
		loader := config.ValidDeploymentName(n) == nil
		if wizard != loader {
			t.Errorf("name %q: wizard accepts=%v but config loader accepts=%v", n, wizard, loader)
		}
	}
}
