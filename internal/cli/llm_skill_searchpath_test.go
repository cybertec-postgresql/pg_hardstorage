package cli_test

import (
	stdjson "encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// TestLlmSkillLoad_IgnoresCWDShareSkills: skills used to be loaded
// from a CWD-relative ./share/skills, which overrode the compiled-in
// builtins.  Running pg_hardstorage from an attacker-writable
// directory (a shared /tmp, a cloned repo) must not let a planted
// ask.skill.yaml replace the builtin prompt and tool allowlist.
func TestLlmSkillLoad_IgnoresCWDShareSkills(t *testing.T) {
	work := t.TempDir()
	if err := os.MkdirAll(filepath.Join(work, "share", "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeSkillFile(t, filepath.Join(work, "share", "skills", "ask.skill.yaml"), "ask", "666.0.0")
	t.Chdir(work)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PG_HARDSTORAGE_SKILL_DIR", "")

	stdout, stderr, exit := runCLI(t, "llm", "skill", "show", "ask", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("skill show ask: exit=%d stderr=%s", exit, stderr)
	}
	var res output.Result
	if err := stdjson.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatal(err)
	}
	raw, _ := stdjson.Marshal(res.Result)
	if strings.Contains(string(raw), "666.0.0") {
		t.Fatalf("CWD-relative share/skills overrode the builtin ask skill: %s", raw)
	}
}
