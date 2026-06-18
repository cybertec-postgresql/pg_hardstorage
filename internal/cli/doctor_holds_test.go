package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// plantHoldMarker writes a hold-marker JSON at the canonical
// path. Mirrors the gap-record fixture in
// TestDoctor_WALGapCheck_FromGapStore — we need ONE hold body
// on disk under the deployment's manifest prefix; ListHolds
// walks suffix-matched keys.
func plantHoldMarker(t *testing.T, repoDir, deployment, backupID, body string) {
	t.Helper()
	dir := filepath.Join(repoDir, "manifests", deployment, "backups", backupID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json.hold"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// configureSingleDeployment writes a minimal pg_hardstorage.yaml
// with one deployment + repo + sets the env for path resolution.
// Same pattern as TestDoctor_WALGapCheck_FromGapStore.
func configureSingleDeployment(t *testing.T, tmp, repoURL string) {
	t.Helper()
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
}

// TestDoctor_ExpiredHold_Surfaced: an expired hold marker
// shows up in doctor's `expired_holds` array + an
// `hold.expired_present` Notice issue with a copy-pasteable
// `hold purge-expired` Suggestion (bulk cleanup is the
// recommended path; single-shot `hold remove` is still valid
// for surgical cases).
func TestDoctor_ExpiredHold_Surfaced(t *testing.T) {
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	repoURL := "file://" + repoDir
	if _, _, exit := runCLI(t, "repo", "init", repoURL); exit != int(output.ExitOK) {
		t.Fatalf("repo init failed")
	}

	// Plant an expired hold — ExpiresAt in 2020.
	plantHoldMarker(t, repoDir, "db1", "db1.full.expired-hold-test", `{
  "schema": "pg_hardstorage.hold.v1",
  "deployment": "db1",
  "backup_id": "db1.full.expired-hold-test",
  "held_at": "2020-01-01T00:00:00Z",
  "holder": "old-debug",
  "reason": "stale-debugging-hold",
  "expires_at": "2020-02-01T00:00:00Z"
}`)

	configureSingleDeployment(t, tmp, repoURL)

	stdout, _, exit := runCLI(t, "doctor", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("doctor exit=%d\n%s", exit, stdout)
	}
	for _, want := range []string{
		`"expired_holds"`,
		`"backup_id": "db1.full.expired-hold-test"`,
		`"holder": "old-debug"`,
		`"reason": "stale-debugging-hold"`,
		`"expired_at": "2020-02-01T00:00:00Z"`,
		`hold.expired_present`,
		`"severity": "notice"`,
		`pg_hardstorage hold purge-expired --yes`,
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("doctor output missing %q:\n%s", want, stdout)
		}
	}
}

// TestDoctor_IndefiniteHold_NotInExpiredList: a hold without
// ExpiresAt is the legal-hold default and never appears in
// `expired_holds` — only bounded + past holds do.
func TestDoctor_IndefiniteHold_NotInExpiredList(t *testing.T) {
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	repoURL := "file://" + repoDir
	if _, _, exit := runCLI(t, "repo", "init", repoURL); exit != int(output.ExitOK) {
		t.Fatalf("repo init failed")
	}

	plantHoldMarker(t, repoDir, "db1", "db1.full.indefinite", `{
  "schema": "pg_hardstorage.hold.v1",
  "deployment": "db1",
  "backup_id": "db1.full.indefinite",
  "held_at": "2026-04-01T00:00:00Z",
  "holder": "compliance",
  "reason": "GDPR-art-17"
}`)

	configureSingleDeployment(t, tmp, repoURL)

	stdout, _, exit := runCLI(t, "doctor", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("doctor exit=%d\n%s", exit, stdout)
	}
	if strings.Contains(stdout, `"expired_holds"`) {
		t.Errorf("indefinite hold should NOT surface in expired_holds:\n%s", stdout)
	}
	if strings.Contains(stdout, "hold.expired_present") {
		t.Errorf("indefinite hold should NOT trigger hold.expired_present:\n%s", stdout)
	}
}

// TestDoctor_ActiveBoundedHold_NotInExpiredList: a bounded
// hold whose ExpiresAt is in the future is still active and
// must not appear in `expired_holds`.
func TestDoctor_ActiveBoundedHold_NotInExpiredList(t *testing.T) {
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	repoURL := "file://" + repoDir
	if _, _, exit := runCLI(t, "repo", "init", repoURL); exit != int(output.ExitOK) {
		t.Fatalf("repo init failed")
	}

	plantHoldMarker(t, repoDir, "db1", "db1.full.active-bounded", `{
  "schema": "pg_hardstorage.hold.v1",
  "deployment": "db1",
  "backup_id": "db1.full.active-bounded",
  "held_at": "2026-04-01T00:00:00Z",
  "holder": "ops",
  "reason": "audit-window-2099",
  "expires_at": "2099-01-01T00:00:00Z"
}`)

	configureSingleDeployment(t, tmp, repoURL)

	stdout, _, exit := runCLI(t, "doctor", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("doctor exit=%d\n%s", exit, stdout)
	}
	if strings.Contains(stdout, `"expired_holds"`) {
		t.Errorf("active bounded hold should not surface as expired:\n%s", stdout)
	}
}

// TestDoctor_ExpiredHold_TextRendering: text mode prints the
// EXPIRED HOLDS block and includes the deployment/backup-id +
// expired_at line for each.
func TestDoctor_ExpiredHold_TextRendering(t *testing.T) {
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	repoURL := "file://" + repoDir
	if _, _, exit := runCLI(t, "repo", "init", repoURL); exit != int(output.ExitOK) {
		t.Fatalf("repo init failed")
	}
	plantHoldMarker(t, repoDir, "db1", "db1.text-mode", `{
  "schema": "pg_hardstorage.hold.v1",
  "deployment": "db1",
  "backup_id": "db1.text-mode",
  "held_at": "2020-01-01T00:00:00Z",
  "holder": "ops",
  "reason": "old",
  "expires_at": "2020-02-01T00:00:00Z"
}`)
	configureSingleDeployment(t, tmp, repoURL)

	stdout, _, exit := runCLI(t, "doctor", "-o", "text")
	if exit != int(output.ExitOK) {
		t.Fatalf("doctor text exit=%d\n%s", exit, stdout)
	}
	for _, want := range []string{
		"EXPIRED HOLDS",
		"db1/db1.text-mode",
		"expired 2020-02-01",
		"holder=ops",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("text output missing %q:\n%s", want, stdout)
		}
	}
}
