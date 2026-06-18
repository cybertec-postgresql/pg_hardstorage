// dockercompose_yaml_test.go — pins that the inline-YAML body in
// docker-compose.yml parses cleanly against our schema.
//
// The reporter of issue #87 hit two bugs at once: the env-var
// path wasn't wired up (now fixed in Load), AND the
// docker-compose.yml's YAML body used keys (`repos:`, `storage:`,
// `encryption:`) that aren't part of our schema.  Even with the
// env-var fix, that body would fail `KnownFields(true)` parsing
// at agent startup.
//
// This test extracts the PG_HARDSTORAGE_CONFIG body from
// docker-compose.yml and parses it against our schema.  A future
// commit that adds a key to the inline YAML without first adding
// it to internal/config.Config fails here at PR time, not at
// docker-compose-up time.

package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
)

func TestDockerComposeYAML_InlineConfigParses(t *testing.T) {
	repoRoot := repoRootFromTestDir(t)
	composePath := filepath.Join(repoRoot, "docker-compose.yml")
	body, err := os.ReadFile(composePath)
	if err != nil {
		t.Fatalf("read %s: %v", composePath, err)
	}

	inlineYAML, ok := composeServiceEnv(t, body, "pg_hardstorage", "PG_HARDSTORAGE_CONFIG")
	if !ok {
		t.Fatalf("docker-compose.yml service pg_hardstorage has no PG_HARDSTORAGE_CONFIG env var")
	}

	// Parse against the real Config struct with KnownFields(true)
	// so unknown keys fail the same way Load does.
	dec := yaml.NewDecoder(strings.NewReader(inlineYAML))
	dec.KnownFields(true)
	var cfg config.Config
	if err := dec.Decode(&cfg); err != nil {
		t.Fatalf("inline YAML body fails to parse against config.Config (issue #87 regression): %v\n--- body ---\n%s",
			err, inlineYAML)
	}

	// Sanity: it must declare at least one deployment so the
	// agent doesn't crash-loop with config.no_deployments.
	if len(cfg.Deployments) == 0 {
		t.Errorf("inline YAML body has no deployments — agent will crash-loop on startup")
	}
}

// TestDockerComposeYAML_InlineConfigLoadsViaEnvVar closes the
// issue #93 loop end-to-end: it feeds the ACTUAL docker-compose.yml
// inline body through the same path the agent uses —
// PG_HARDSTORAGE_CONFIG → config.Load → Deployments — and asserts a
// non-empty deployment set.
//
// TestDockerComposeYAML_InlineConfigParses above only decodes the
// body in isolation; it would stay green even if Load's env-var
// branch regressed.  The reporter of #93 hit exactly that runtime
// path (agent calls Load, gets zero deployments, exits with
// config.no_deployments).  This test exercises it: a regression in
// EITHER the compose body OR Load's env-var wiring fails here at PR
// time, not at `docker compose up` time.
func TestDockerComposeYAML_InlineConfigLoadsViaEnvVar(t *testing.T) {
	repoRoot := repoRootFromTestDir(t)
	composePath := filepath.Join(repoRoot, "docker-compose.yml")
	body, err := os.ReadFile(composePath)
	if err != nil {
		t.Fatalf("read %s: %v", composePath, err)
	}

	inlineYAML, ok := composeServiceEnv(t, body, "pg_hardstorage", "PG_HARDSTORAGE_CONFIG")
	if !ok {
		t.Fatalf("docker-compose.yml service pg_hardstorage has no PG_HARDSTORAGE_CONFIG env var")
	}

	// Empty config dir → no on-disk pg_hardstorage.yaml, so the
	// only deployment source is the env var, exactly like the
	// distroless container the operator runs.
	p := pathsForTempDir(t, t.TempDir())
	t.Setenv("PG_HARDSTORAGE_CONFIG", inlineYAML)

	res, err := config.Load(p)
	if err != nil {
		t.Fatalf("Load with the compose body in PG_HARDSTORAGE_CONFIG failed (issue #93 regression): %v\n--- body ---\n%s", err, inlineYAML)
	}
	if len(res.Config.Deployments) == 0 {
		t.Fatalf("Load returned zero deployments from the compose body — agent crash-loops with config.no_deployments (issue #93)\n--- body ---\n%s", inlineYAML)
	}

	// Tie the assertion to the actual demo deployment shape, not
	// just a non-empty map: a body that parsed but lost its
	// connection/repo would still crash the agent at run time even
	// though Load returned "a deployment".
	demo, ok := res.Config.Deployments["demo"]
	if !ok {
		t.Fatalf("compose body loaded but has no \"demo\" deployment; got %v", mapKeys(res.Config.Deployments))
	}
	if demo.PGConnection == "" || demo.Repo == "" {
		t.Errorf("demo deployment is missing pg_connection or repo: pg_connection=%q repo=%q", demo.PGConnection, demo.Repo)
	}
}

// composeServiceEnv parses docker-compose.yml as YAML — the same
// structure Docker resolves — and returns the value of envVar in
// the named service's `environment:` block.
//
// Earlier this was a hand-rolled line-walker that scanned for
// `<varName>: |` anywhere in the file.  That had two problems: it
// re-implemented YAML literal-block dedentation (so it could
// extract a body subtly different from the one Docker injects),
// and it wasn't anchored to a service, so it would happily match a
// PG_HARDSTORAGE_CONFIG declared on some OTHER service.  Decoding
// the real document instead means the test reads the EXACT string
// the agent container receives and survives reindentation, comment
// churn, or key reordering in the compose file.
//
// environment: is decoded as map[string]string, which is the map
// form this compose file uses (`KEY: value`).  The list form
// (`- KEY=value`) would need separate handling; a Fatalf guards
// that so a future switch to the list form fails loudly here
// rather than silently reporting "no env var".
func composeServiceEnv(t *testing.T, compose []byte, service, envVar string) (string, bool) {
	t.Helper()
	var doc struct {
		Services map[string]struct {
			Environment map[string]string `yaml:"environment"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(compose, &doc); err != nil {
		t.Fatalf("parse docker-compose.yml as YAML: %v", err)
	}
	svc, ok := doc.Services[service]
	if !ok {
		t.Fatalf("docker-compose.yml has no %q service (have %v)", service, mapKeys(doc.Services))
	}
	val, ok := svc.Environment[envVar]
	return val, ok
}

func mapKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

// repoRootFromTestDir walks up from the package's test directory
// until it finds a go.mod, returning that directory.  Mirror of
// the helper in error_class_audit_test.go.
func repoRootFromTestDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 10; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not find repo root (no go.mod up the tree)")
	return ""
}
