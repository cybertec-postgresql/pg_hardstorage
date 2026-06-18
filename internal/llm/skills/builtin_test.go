package skills_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/skills"
)

func TestLoadBuiltins_MinimumSet(t *testing.T) {
	set, err := skills.LoadBuiltins()
	if err != nil {
		t.Fatalf("LoadBuiltins: %v", err)
	}
	want := []string{"ask", "explain", "restore", "incident"}
	for _, name := range want {
		s, err := set.Get(name)
		if err != nil {
			t.Errorf("expected builtin skill %q: %v", name, err)
			continue
		}
		if !strings.HasPrefix(s.Source, "builtin:") {
			t.Errorf("skill %s should declare a builtin source; got %q", name, s.Source)
		}
		if s.PromptTemplate == "" {
			t.Errorf("skill %s missing prompt_template", name)
		}
		if !s.Permissions.ReadOnly {
			t.Errorf("skill %s should be read-only", name)
		}
	}
}

func TestLoadBuiltins_AllAreLintClean(t *testing.T) {
	set, err := skills.LoadBuiltins()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range set.All() {
		issues := s.Lint()
		if len(issues) > 0 {
			t.Errorf("builtin skill %s has lint issues: %v", s.Name, issues)
		}
	}
}

func TestLoadAllWithBuiltins_OverridesWin(t *testing.T) {
	// Override "ask" via a temp dir.
	dir := t.TempDir()
	override := `schema: pg_hardstorage.skill.v1
name: ask
version: 9.9.9
description: operator-overridden
prompt_template: overridden
permissions:
  read_only: true
guardrails:
  - max_token_budget_per_session: 1000
context:
  available_tools:
    - search_docs
`
	if err := writeFile(filepath.Join(dir, "ask.skill.yaml"), override); err != nil {
		t.Fatal(err)
	}
	set, err := skills.LoadAllWithBuiltins([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	got, err := set.Get("ask")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "9.9.9" {
		t.Errorf("override should win: version = %q, want 9.9.9", got.Version)
	}
	if !strings.Contains(got.PromptTemplate, "overridden") {
		t.Errorf("prompt_template not overridden: %q", got.PromptTemplate)
	}
	// Other builtins should still be present.
	for _, name := range []string{"explain", "restore", "incident"} {
		if _, err := set.Get(name); err != nil {
			t.Errorf("builtin %s lost when override only touched 'ask': %v", name, err)
		}
	}
}

func writeFile(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o600)
}
