package cli_test

import (
	stdjson "encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// TestAuditAnchor_RoundTrip drives the full operator-visible flow:
// repo init → audit append (chain has at least one event) → anchor →
// verify-anchor (OK).
func TestAuditAnchor_RoundTrip(t *testing.T) {
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	repoURL := "file://" + repoDir

	if _, _, exit := runCLI(t, "repo", "init", repoURL); exit != int(output.ExitOK) {
		t.Fatalf("repo init failed")
	}

	// Plant an audit event so the chain isn't empty.
	if _, _, exit := runCLI(t,
		"audit", "append", "operator.test",
		"--repo", repoURL,
		"--actor", "alice",
		"-o", "json",
	); exit != int(output.ExitOK) {
		t.Fatalf("audit append failed")
	}

	// Anchor the chain head.
	stdout, stderr, exit := runCLI(t,
		"audit", "anchor",
		"--repo", repoURL,
		"--publisher", "test-node",
		"-o", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("audit anchor: exit=%d\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}
	var anchorRes output.Result
	if err := stdjson.Unmarshal([]byte(stdout), &anchorRes); err != nil {
		t.Fatalf("decode anchor result: %v\n%s", err, stdout)
	}
	resultMap := anchorRes.Result.(map[string]any)
	logID, _ := resultMap["log_id"].(string)
	if logID == "" {
		t.Fatalf("no log_id in anchor result: %+v", anchorRes.Result)
	}
	if resultMap["publisher_id"] != "test-node" {
		t.Errorf("publisher_id = %v", resultMap["publisher_id"])
	}

	// Verify the anchor — should succeed (OK=true).
	stdout, stderr, exit = runCLI(t,
		"audit", "verify-anchor", logID,
		"--repo", repoURL,
		"-o", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("verify-anchor (clean chain): exit=%d\nstdout=%s\nstderr=%s",
			exit, stdout, stderr)
	}
	if !strings.Contains(stdout, `"ok": true`) {
		t.Errorf("expected OK=true in result: %s", stdout)
	}

	// Re-anchor the same chain head — should be idempotent (same logID).
	stdout, _, exit = runCLI(t,
		"audit", "anchor",
		"--repo", repoURL,
		"--publisher", "another-node",
		"-o", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("re-anchor failed: %s", stdout)
	}
	var second output.Result
	stdjson.Unmarshal([]byte(stdout), &second)
	if second.Result.(map[string]any)["log_id"] != logID {
		t.Errorf("re-anchor should yield same logID; got %v vs %s",
			second.Result.(map[string]any)["log_id"], logID)
	}
}

// TestAuditAnchor_RefusesEmptyChain: calling anchor on a fresh repo
// (no events yet) surfaces a structured error.
func TestAuditAnchor_RefusesEmptyChain(t *testing.T) {
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	repoURL := "file://" + repoDir
	if _, _, exit := runCLI(t, "repo", "init", repoURL); exit != int(output.ExitOK) {
		t.Fatalf("repo init failed")
	}

	_, stderr, exit := runCLI(t,
		"audit", "anchor",
		"--repo", repoURL,
		"-o", "json",
	)
	if exit == int(output.ExitOK) {
		t.Fatalf("anchor should fail on empty chain; stderr=%s", stderr)
	}
	if !strings.Contains(stderr, "audit.anchor_failed") {
		t.Errorf("expected audit.anchor_failed code; stderr=%s", stderr)
	}
	if !strings.Contains(stderr, "empty") {
		t.Errorf("error should mention empty chain; stderr=%s", stderr)
	}
}
