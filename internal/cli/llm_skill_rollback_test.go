package cli_test

import (
	stdjson "encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// helper: write a minimal valid skill yaml to <path> with the
// supplied name + version
func writeSkillFile(t *testing.T, path, name, version string) {
	t.Helper()
	body := `schema: pg_hardstorage.skill.v1
name: ` + name + `
display_name: ` + name + `
version: ` + version + `
description: |
  Test skill ` + name + ` ` + version + `
permissions:
  read_only: true
context:
  available_tools:
    - read_doctor
guardrails:
  - max_token_budget_per_session: 4000
prompt_template: |
  Hello.
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestLlmSkillInstall_FirstInstallNoSnapshot:
// installing a fresh skill into an empty directory writes the
// file and reports no snapshot path.
func TestLlmSkillInstall_FirstInstallNoSnapshot(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PG_HARDSTORAGE_SKILL_DIR", dir)

	src := filepath.Join(t.TempDir(), "src.skill.yaml")
	writeSkillFile(t, src, "myskill", "1.0.0")

	stdout, stderr, exit := runCLI(t, "llm", "skill", "install", src, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit=%d stderr=%s", exit, stderr)
	}

	var res output.Result
	if err := stdjson.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatal(err)
	}
	body := res.Result.(map[string]any)
	if body["name"] != "myskill" {
		t.Errorf("name = %v", body["name"])
	}
	if body["version"] != "1.0.0" {
		t.Errorf("version = %v", body["version"])
	}
	if body["snapshot_path"] != nil && body["snapshot_path"] != "" {
		t.Errorf("first install should have no snapshot; got %v", body["snapshot_path"])
	}

	// Active file exists, no snapshot files yet.
	if _, err := os.Stat(filepath.Join(dir, "myskill.skill.yaml")); err != nil {
		t.Errorf("active file missing: %v", err)
	}
}

// TestLlmSkillInstall_SecondInstallSnapshotsPrevious:
// installing a second time over an existing skill snapshots the
// old file under <name>.skill.yaml.<rfc3339-without-colons>.
func TestLlmSkillInstall_SecondInstallSnapshotsPrevious(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PG_HARDSTORAGE_SKILL_DIR", dir)
	src := filepath.Join(t.TempDir(), "src.skill.yaml")

	// Install v1
	writeSkillFile(t, src, "myskill", "1.0.0")
	if _, _, exit := runCLI(t, "llm", "skill", "install", src); exit != int(output.ExitOK) {
		t.Fatal("install v1 failed")
	}
	// Install v2
	writeSkillFile(t, src, "myskill", "2.0.0")
	stdout, _, exit := runCLI(t, "llm", "skill", "install", src, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatal("install v2 failed")
	}
	var res output.Result
	stdjson.Unmarshal([]byte(stdout), &res)
	body := res.Result.(map[string]any)
	snap, _ := body["snapshot_path"].(string)
	if snap == "" {
		t.Error("second install should produce a snapshot_path")
	}
	if !strings.HasPrefix(filepath.Base(snap), "myskill.skill.yaml.") {
		t.Errorf("snapshot filename wrong: %s", snap)
	}
}

// TestLlmSkillRollback_RestoresPreviousVersion:
// after install v1 + install v2, rollback should bring v1 back
// to the active file.
func TestLlmSkillRollback_RestoresPreviousVersion(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PG_HARDSTORAGE_SKILL_DIR", dir)
	src := filepath.Join(t.TempDir(), "src.skill.yaml")

	writeSkillFile(t, src, "myskill", "1.0.0")
	runCLI(t, "llm", "skill", "install", src)
	writeSkillFile(t, src, "myskill", "2.0.0")
	runCLI(t, "llm", "skill", "install", src)

	stdout, stderr, exit := runCLI(t, "llm", "skill", "rollback", "myskill", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("rollback exit=%d stderr=%s", exit, stderr)
	}
	var res output.Result
	stdjson.Unmarshal([]byte(stdout), &res)
	body := res.Result.(map[string]any)
	if body["now_installed_version"] != "1.0.0" {
		t.Errorf("expected v1.0.0 after rollback; got %v", body["now_installed_version"])
	}

	// Active file should now contain v1.0.0
	active, err := os.ReadFile(filepath.Join(dir, "myskill.skill.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(active), "version: 1.0.0") {
		t.Errorf("active file should be v1; got\n%s", active)
	}
}

// TestLlmSkillRollback_NoSnapshotsRefuses:
// rolling back a skill without any snapshots should refuse with
// notfound.skill_snapshot.
func TestLlmSkillRollback_NoSnapshotsRefuses(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PG_HARDSTORAGE_SKILL_DIR", dir)

	// Install v1 only — no previous version exists.
	src := filepath.Join(t.TempDir(), "src.skill.yaml")
	writeSkillFile(t, src, "myskill", "1.0.0")
	runCLI(t, "llm", "skill", "install", src)

	_, stderr, exit := runCLI(t, "llm", "skill", "rollback", "myskill", "-o", "json")
	if exit == int(output.ExitOK) {
		t.Fatal("rollback without snapshots should refuse")
	}
	if !strings.Contains(stderr, "notfound.skill_snapshot") {
		t.Errorf("expected notfound.skill_snapshot; stderr=%s", stderr)
	}
}

// TestLlmSkillHistory_ListsSnapshots:
// after multiple installs, history reports the snapshots.
func TestLlmSkillHistory_ListsSnapshots(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PG_HARDSTORAGE_SKILL_DIR", dir)
	src := filepath.Join(t.TempDir(), "src.skill.yaml")

	for _, v := range []string{"1.0.0", "1.1.0", "2.0.0"} {
		writeSkillFile(t, src, "myskill", v)
		runCLI(t, "llm", "skill", "install", src)
	}

	stdout, _, exit := runCLI(t, "llm", "skill", "history", "myskill", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatal("history should succeed")
	}
	var res output.Result
	stdjson.Unmarshal([]byte(stdout), &res)
	body := res.Result.(map[string]any)
	snaps := body["snapshots"].([]any)
	if len(snaps) != 2 {
		// 3 installs → 2 snapshots (v1 → v1.1 snaps v1; v1.1 → v2 snaps v1.1)
		t.Errorf("expected 2 snapshots, got %d: %v", len(snaps), snaps)
	}
}
