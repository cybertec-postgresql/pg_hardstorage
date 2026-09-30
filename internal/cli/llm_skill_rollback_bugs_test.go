package cli_test

import (
	stdjson "encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// TestLlmSkillInstall_RejectsPathTraversalName: the YAML `name:`
// field becomes part of the destination path, so a name like
// `../../x` must be refused before anything is written — otherwise
// install writes outside the overlay directory.
func TestLlmSkillInstall_RejectsPathTraversalName(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "a", "b", "skills")
	t.Setenv("PG_HARDSTORAGE_SKILL_DIR", dir)

	for _, bad := range []string{"../../x", "sub/x", "X", ".hidden", "a.b", "-x"} {
		src := filepath.Join(t.TempDir(), "src.skill.yaml")
		writeSkillFile(t, src, `"`+bad+`"`, "1.0.0")
		_, stderr, exit := runCLI(t, "llm", "skill", "install", src)
		if exit == int(output.ExitOK) {
			t.Errorf("install of name %q succeeded; want refusal", bad)
		}
		if !strings.Contains(stderr, "llm.skill_invalid_name") {
			t.Errorf("name %q: want llm.skill_invalid_name; stderr=%s", bad, stderr)
		}
	}
	// Nothing may have escaped the overlay directory.
	if _, err := os.Stat(filepath.Join(root, "x.skill.yaml")); err == nil {
		t.Fatal("traversal name wrote outside the overlay directory")
	}
	if _, err := os.Stat(filepath.Join(root, "a", "x.skill.yaml")); err == nil {
		t.Fatal("traversal name wrote outside the overlay directory")
	}
}

// TestLlmSkillRollback_RejectsInvalidName: rollback/history take the
// name from argv and join it into paths too.
func TestLlmSkillRollback_RejectsInvalidName(t *testing.T) {
	t.Setenv("PG_HARDSTORAGE_SKILL_DIR", t.TempDir())
	for _, sub := range []string{"rollback", "history"} {
		_, stderr, exit := runCLI(t, "llm", "skill", sub, "../x")
		if exit == int(output.ExitOK) || !strings.Contains(stderr, "llm.skill_invalid_name") {
			t.Errorf("%s ../x: exit=%d stderr=%s; want llm.skill_invalid_name", sub, exit, stderr)
		}
	}
}

// TestLlmSkillRollback_SecondRollbackGoesFurtherBack: install
// v1, v2, v3 then roll back twice.  The first rollback must land
// on v2, the second on v1 — not back on v3 (the archive of the
// rolled-from file must never become a rollback candidate).
func TestLlmSkillRollback_SecondRollbackGoesFurtherBack(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PG_HARDSTORAGE_SKILL_DIR", dir)
	src := filepath.Join(t.TempDir(), "src.skill.yaml")
	for _, v := range []string{"1.0.0", "2.0.0", "3.0.0"} {
		writeSkillFile(t, src, "myskill", v)
		if _, stderr, exit := runCLI(t, "llm", "skill", "install", src); exit != int(output.ExitOK) {
			t.Fatalf("install %s: %s", v, stderr)
		}
	}
	for _, want := range []string{"2.0.0", "1.0.0"} {
		stdout, stderr, exit := runCLI(t, "llm", "skill", "rollback", "myskill", "-o", "json")
		if exit != int(output.ExitOK) {
			t.Fatalf("rollback to %s: exit=%d stderr=%s", want, exit, stderr)
		}
		var res output.Result
		if err := stdjson.Unmarshal([]byte(stdout), &res); err != nil {
			t.Fatal(err)
		}
		body := res.Result.(map[string]any)
		if got := body["now_installed_version"]; got != want {
			t.Fatalf("rollback: now_installed_version=%v, want %s", got, want)
		}
		// The pre-rollback archive must still exist so the operator
		// can re-install it by hand.
		if p, _ := body["post_rollback_snapshot"].(string); p == "" {
			t.Error("rollback did not archive the replaced file")
		} else if _, err := os.Stat(p); err != nil {
			t.Errorf("archived file missing: %v", err)
		}
	}
	// History exhausted: a third rollback has nothing older.
	if _, _, exit := runCLI(t, "llm", "skill", "rollback", "myskill"); exit == int(output.ExitOK) {
		t.Error("third rollback succeeded; want notfound (no older version left)")
	}
}

// TestLlmSkillRollback_IgnoresNonSnapshotFiles: only files that
// match the exact snapshot naming are rollback candidates.  A stray
// `<name>.skill.yaml.tmp` left by an interrupted atomic write (or
// any other suffix) sorts after the timestamped snapshots and must
// not be restored.
func TestLlmSkillRollback_IgnoresNonSnapshotFiles(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PG_HARDSTORAGE_SKILL_DIR", dir)
	src := filepath.Join(t.TempDir(), "src.skill.yaml")
	for _, v := range []string{"1.0.0", "2.0.0"} {
		writeSkillFile(t, src, "myskill", v)
		runCLI(t, "llm", "skill", "install", src)
	}
	writeSkillFile(t, filepath.Join(dir, "myskill.skill.yaml.tmp"), "myskill", "9.9.9")
	writeSkillFile(t, filepath.Join(dir, "myskill.skill.yaml.zzz"), "myskill", "8.8.8")

	stdout, _, exit := runCLI(t, "llm", "skill", "history", "myskill", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatal("history failed")
	}
	var res output.Result
	stdjson.Unmarshal([]byte(stdout), &res)
	if snaps := res.Result.(map[string]any)["snapshots"].([]any); len(snaps) != 1 {
		t.Errorf("history lists non-snapshot files: %v", snaps)
	}

	stdout, stderr, exit := runCLI(t, "llm", "skill", "rollback", "myskill", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("rollback exit=%d stderr=%s", exit, stderr)
	}
	stdjson.Unmarshal([]byte(stdout), &res)
	if got := res.Result.(map[string]any)["now_installed_version"]; got != "1.0.0" {
		t.Errorf("rollback picked a non-snapshot file: now_installed_version=%v", got)
	}
}
