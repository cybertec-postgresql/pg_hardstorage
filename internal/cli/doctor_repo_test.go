package cli_test

import (
	stdjson "encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// TestDoctor_RepoCheck_FreshRepoIsHealthy: a freshly-init'd repo
// has no audit events and no anchor. Doctor reports the repo as
// reachable with anchor_fresh=true (empty chain is healthy by
// definition).
func TestDoctor_RepoCheck_FreshRepoIsHealthy(t *testing.T) {
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	repoURL := "file://" + repoDir
	if _, _, exit := runCLI(t, "repo", "init", repoURL); exit != int(output.ExitOK) {
		t.Fatalf("repo init failed")
	}

	cfgDir := filepath.Join(tmp, "config")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(cfgDir, "pg_hardstorage.yaml")
	if err := os.WriteFile(cfgPath, []byte(
		"deployments:\n  db1:\n    pg_connection: postgres://x\n    repo: "+repoURL+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PG_HARDSTORAGE_CONFIG_DIR", cfgDir)
	t.Setenv("PG_HARDSTORAGE_KEYRING_DIR", filepath.Join(tmp, "keyring"))
	t.Setenv("PG_HARDSTORAGE_STATE_DIR", filepath.Join(tmp, "state"))
	t.Setenv("PG_HARDSTORAGE_CACHE_DIR", filepath.Join(tmp, "cache"))
	t.Setenv("PG_HARDSTORAGE_LOG_DIR", filepath.Join(tmp, "log"))
	t.Setenv("PG_HARDSTORAGE_RUNTIME_DIR", filepath.Join(tmp, "run"))

	stdout, stderr, exit := runCLI(t, "doctor", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("doctor: exit=%d\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}
	var res output.Result
	if err := stdjson.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout)
	}

	body, _ := stdjson.Marshal(res.Result)
	bodyStr := string(body)
	if !strings.Contains(bodyStr, repoURL) {
		t.Errorf("doctor result missing repo URL %q: %s", repoURL, bodyStr)
	}
	if !strings.Contains(bodyStr, `"reachable":true`) {
		t.Errorf("repo should be reachable: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, `"anchor_fresh":true`) {
		t.Errorf("fresh repo should have anchor_fresh=true: %s", bodyStr)
	}
}

// TestDoctor_RepoCheck_StaleAnchor: a repo with audit events but no
// anchor surfaces audit.anchor_missing as a warning issue.
func TestDoctor_RepoCheck_StaleAnchor(t *testing.T) {
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	repoURL := "file://" + repoDir
	if _, _, exit := runCLI(t, "repo", "init", repoURL); exit != int(output.ExitOK) {
		t.Fatalf("repo init failed")
	}

	// Plant an audit event but skip anchoring → chain has 1 event,
	// 0 anchors. Doctor should flag this.
	if _, _, exit := runCLI(t,
		"audit", "append", "operator.test",
		"--repo", repoURL,
		"-o", "json",
	); exit != int(output.ExitOK) {
		t.Fatalf("audit append failed")
	}

	cfgDir := filepath.Join(tmp, "config")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(cfgDir, "pg_hardstorage.yaml")
	if err := os.WriteFile(cfgPath, []byte(
		"deployments:\n  db1:\n    pg_connection: postgres://x\n    repo: "+repoURL+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PG_HARDSTORAGE_CONFIG_DIR", cfgDir)
	t.Setenv("PG_HARDSTORAGE_KEYRING_DIR", filepath.Join(tmp, "keyring"))
	t.Setenv("PG_HARDSTORAGE_STATE_DIR", filepath.Join(tmp, "state"))
	t.Setenv("PG_HARDSTORAGE_CACHE_DIR", filepath.Join(tmp, "cache"))
	t.Setenv("PG_HARDSTORAGE_LOG_DIR", filepath.Join(tmp, "log"))
	t.Setenv("PG_HARDSTORAGE_RUNTIME_DIR", filepath.Join(tmp, "run"))

	stdout, _, _ := runCLI(t, "doctor", "-o", "json")
	if !strings.Contains(stdout, "audit.anchor_missing") {
		t.Errorf("expected audit.anchor_missing issue: %s", stdout)
	}
}

// TestDoctor_RepoCheck_FreshAnchor: a repo with audit + anchor at
// the latest event has anchor_fresh=true and no issue.
func TestDoctor_RepoCheck_FreshAnchor(t *testing.T) {
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	repoURL := "file://" + repoDir
	if _, _, exit := runCLI(t, "repo", "init", repoURL); exit != int(output.ExitOK) {
		t.Fatalf("repo init failed")
	}
	if _, _, exit := runCLI(t,
		"audit", "append", "operator.test",
		"--repo", repoURL,
		"-o", "json",
	); exit != int(output.ExitOK) {
		t.Fatalf("audit append failed")
	}
	if _, _, exit := runCLI(t,
		"audit", "anchor",
		"--repo", repoURL,
		"-o", "json",
	); exit != int(output.ExitOK) {
		t.Fatalf("audit anchor failed")
	}

	cfgDir := filepath.Join(tmp, "config")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(cfgDir, "pg_hardstorage.yaml")
	if err := os.WriteFile(cfgPath, []byte(
		"deployments:\n  db1:\n    pg_connection: postgres://x\n    repo: "+repoURL+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PG_HARDSTORAGE_CONFIG_DIR", cfgDir)
	t.Setenv("PG_HARDSTORAGE_KEYRING_DIR", filepath.Join(tmp, "keyring"))
	t.Setenv("PG_HARDSTORAGE_STATE_DIR", filepath.Join(tmp, "state"))
	t.Setenv("PG_HARDSTORAGE_CACHE_DIR", filepath.Join(tmp, "cache"))
	t.Setenv("PG_HARDSTORAGE_LOG_DIR", filepath.Join(tmp, "log"))
	t.Setenv("PG_HARDSTORAGE_RUNTIME_DIR", filepath.Join(tmp, "run"))

	stdout, _, _ := runCLI(t, "doctor", "-o", "json")
	if !strings.Contains(stdout, `"anchor_fresh": true`) {
		t.Errorf("expected anchor_fresh=true after audit anchor: %s", stdout)
	}
	if strings.Contains(stdout, "audit.anchor_missing") || strings.Contains(stdout, "audit.anchor_stale") {
		t.Errorf("no anchor issue should fire on a freshly-anchored chain: %s", stdout)
	}
}

// TestDoctor_WALGapCheck_FromGapStore: when a gap record is
// persisted to the repo's wal/<deployment>/gaps/ prefix, doctor
// surfaces a wal.gap_persistent issue + a walGapReport entry
// for that deployment. Validates the v0.6+ gap-state-survives-
// agent-restart story end-to-end.
func TestDoctor_WALGapCheck_FromGapStore(t *testing.T) {
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	repoURL := "file://" + repoDir
	if _, _, exit := runCLI(t, "repo", "init", repoURL); exit != int(output.ExitOK) {
		t.Fatalf("repo init failed")
	}

	// Plant a synthetic gap record by writing the JSON directly
	// at the canonical key. The gap-store package's tests
	// already validate Put → List round-trip; here we just need
	// SOMETHING under the prefix so doctor's check fires.
	gapDir := filepath.Join(repoDir, "wal", "db1", "gaps")
	if err := os.MkdirAll(gapDir, 0o755); err != nil {
		t.Fatal(err)
	}
	gapBody := `{
  "schema": "pg_hardstorage.wal.gap.v1",
  "deployment": "db1",
  "slot_name": "pg_hardstorage_db1",
  "slot_role": "leader",
  "timeline": 7,
  "gap_start_lsn": "0/3000028",
  "gap_end_lsn": "0/30001A0",
  "gap_bytes": 420,
  "detected_at": "2026-04-30T12:00:00Z"
}`
	if err := os.WriteFile(filepath.Join(gapDir, "7-1234567890.json"), []byte(gapBody), 0o644); err != nil {
		t.Fatal(err)
	}

	cfgDir := filepath.Join(tmp, "config")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(cfgDir, "pg_hardstorage.yaml")
	if err := os.WriteFile(cfgPath, []byte(
		"deployments:\n  db1:\n    pg_connection: postgres://x\n    repo: "+repoURL+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PG_HARDSTORAGE_CONFIG_DIR", cfgDir)
	t.Setenv("PG_HARDSTORAGE_KEYRING_DIR", filepath.Join(tmp, "keyring"))
	t.Setenv("PG_HARDSTORAGE_STATE_DIR", filepath.Join(tmp, "state"))
	t.Setenv("PG_HARDSTORAGE_CACHE_DIR", filepath.Join(tmp, "cache"))
	t.Setenv("PG_HARDSTORAGE_LOG_DIR", filepath.Join(tmp, "log"))
	t.Setenv("PG_HARDSTORAGE_RUNTIME_DIR", filepath.Join(tmp, "run"))

	stdout, _, exit := runCLI(t, "doctor", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("doctor exit=%d\n%s", exit, stdout)
	}
	for _, want := range []string{
		`wal.gap_persistent`,                // issue code
		`"deployment": "db1"`,               // walGapReport entry
		`"gap_bytes": 420`,                  // value preserved
		`"slot_name": "pg_hardstorage_db1"`, // slot identity
		`"timeline": 7`,                     // TLI populated
		`pg_hardstorage repair slot db1`,    // suggestion command
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("doctor output missing %q:\n%s", want, stdout)
		}
	}
}
