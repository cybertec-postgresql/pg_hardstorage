//go:build integration

package runner_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/runner"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/scenario"
)

// TestRunner_L1Smoke runs the L1 smoke scenario end-to-end against a
// real PG via testcontainers-go. This is the integration-tier gate
// for the testkit itself — if this passes, the topology + load engine
// + assertion DSL + runner round-trip is intact for the L1/L2 CI
// tiers that gate every PR.
//
// Build-tagged `integration` so `go test ./...` (no tag) keeps unit
// tests fast and Docker-free.
func TestRunner_L1Smoke(t *testing.T) {
	if testing.Short() {
		t.Skip("L1 smoke spins up a PG container; -short")
	}

	// The scenario file at the repo root references the load file
	// with a repo-relative path; resolve them against this test's
	// working directory.
	repoRoot, err := repoRoot()
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}
	scenarioPath := filepath.Join(repoRoot, "test/scenarios/L1_smoke.scenario.yaml")
	loadPath := filepath.Join(repoRoot, "test/load/oltp_smoke.load.yaml")

	// The scenario's `load.file` is repo-relative; rewrite it to the
	// absolute path post-parse so the runner can find it from any cwd.
	sc, err := scenario.FromFile(scenarioPath)
	if err != nil {
		t.Fatalf("parse scenario: %v", err)
	}
	sc.Load.File = loadPath

	// Bring up the topology, run the scenario, tear down. Generous
	// timeout for the testcontainers PG bring-up on a cold runner.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	res, err := runner.Run(ctx, sc, runner.RunOptions{
		Out: testWriter{t},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Pass {
		t.Fatalf("scenario did not pass: %s", res.Failure)
	}
	t.Logf("L1 smoke: %d steps, %d assertions, %d ms",
		len(res.StepResults), len(res.AssertResults), res.DurationMS)
}

// repoRoot walks up from the cwd looking for the go.mod file. Avoids
// hard-coding a path that would break when the test runs in CI vs
// locally. Found-no-go.mod is a fatal misconfiguration.
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

// testWriter forwards NDJSON progress events to t.Log so a failing
// run leaves a readable transcript in the CI output, without
// allocating a separate logger.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", p)
	return len(p), nil
}
