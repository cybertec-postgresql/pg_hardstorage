package scenario_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/scenario"
)

// TestAllScenarioFilesParse is a CI safety net: every
// test/scenarios/*.scenario.yaml must parse + pass scenario.Parse's
// structural validation (schema pin, required name/topology/steps,
// KnownFields=true so a typo'd key is rejected, assert-list parsing).
//
// Without this, a malformed scenario only surfaces when that specific
// scenario is run by the integration harness — which most scenarios
// are not, on most CI runs. A newly-added scenario (e.g. the issue-#99
// L3_restore_to_lsn_unreachable and the L3_partial_dump_database_flag
// PITR/partial scenarios) could ship a schema typo and go unnoticed.
// Parsing is cheap; do it for the whole corpus on every run.
func TestAllScenarioFilesParse(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}
	dir := filepath.Join(root, "test", "scenarios")
	files, err := filepath.Glob(filepath.Join(dir, "*.scenario.yaml"))
	if err != nil {
		t.Fatalf("glob scenarios: %v", err)
	}
	if len(files) == 0 {
		t.Fatalf("no *.scenario.yaml files found under %s", dir)
	}
	t.Logf("validating %d scenario files", len(files))
	for _, f := range files {
		name := filepath.Base(f)
		t.Run(name, func(t *testing.T) {
			body, err := os.ReadFile(f)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			s, err := scenario.Parse(body)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if s.Name == "" {
				t.Error("parsed scenario has empty Name")
			}
		})
	}
}

// repoRoot walks up from the package dir looking for go.mod.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}
