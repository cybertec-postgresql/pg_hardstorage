package skills_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/skills"
)

const minimalSkill = `schema: pg_hardstorage.skill.v1
name: ask
version: 0.1.0
description: minimal test skill
permissions:
  read_only: true
context:
  available_tools: []
guardrails:
  - max_token_budget_per_session: 4000
prompt_template: |
  you are a test
`

func TestParse_Minimal(t *testing.T) {
	s, err := skills.Parse([]byte(minimalSkill))
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "ask" {
		t.Errorf("Name = %q", s.Name)
	}
	if s.Version != "0.1.0" {
		t.Errorf("Version = %q", s.Version)
	}
	if !s.Permissions.ReadOnly {
		t.Error("ReadOnly should be true")
	}
}

func TestParse_RejectsBadSchema(t *testing.T) {
	body := []byte(`schema: wrong
name: x
version: 1
prompt_template: x
`)
	if _, err := skills.Parse(body); err == nil || !strings.Contains(err.Error(), "schema") {
		t.Errorf("expected schema error; got %v", err)
	}
}

func TestParse_RejectsMissingFields(t *testing.T) {
	body := []byte(`schema: pg_hardstorage.skill.v1
name: x
prompt_template: x
`)
	if _, err := skills.Parse(body); err == nil {
		t.Error("expected version-required error")
	}
}

func TestLint_FlagsMissingDescription(t *testing.T) {
	body := []byte(`schema: pg_hardstorage.skill.v1
name: ask
version: 0.1.0
permissions:
  read_only: true
guardrails:
  - max_token_budget_per_session: 4000
prompt_template: x
`)
	s, err := skills.Parse(body)
	if err != nil {
		t.Fatal(err)
	}
	issues := s.Lint()
	found := false
	for _, iss := range issues {
		if strings.Contains(iss, "description") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected description warning; got %v", issues)
	}
}

func TestLint_FlagsExecuteCommand(t *testing.T) {
	body := []byte(`schema: pg_hardstorage.skill.v1
name: ask
version: 0.1.0
description: x
permissions:
  read_only: false
context:
  available_tools:
    - execute_command
guardrails:
  - max_token_budget_per_session: 1000
prompt_template: x
`)
	s, err := skills.Parse(body)
	if err != nil {
		t.Fatal(err)
	}
	issues := s.Lint()
	wantHits := []string{"read_only", "execute_command"}
	for _, want := range wantHits {
		found := false
		for _, iss := range issues {
			if strings.Contains(iss, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("expected lint to flag %q; got %v", want, issues)
		}
	}
}

func TestLoadAll_Precedence(t *testing.T) {
	tmp := t.TempDir()
	low := filepath.Join(tmp, "low")
	high := filepath.Join(tmp, "high")
	if err := os.MkdirAll(low, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(high, 0o755); err != nil {
		t.Fatal(err)
	}
	// Same name in both dirs; high should win.
	if err := os.WriteFile(filepath.Join(low, "ask.skill.yaml"),
		[]byte(replaceVersion(minimalSkill, "low-version")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(high, "ask.skill.yaml"),
		[]byte(replaceVersion(minimalSkill, "high-version")), 0o644); err != nil {
		t.Fatal(err)
	}
	set, err := skills.LoadAll([]string{low, high})
	if err != nil {
		t.Fatal(err)
	}
	got, err := set.Get("ask")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "high-version" {
		t.Errorf("Version = %q; want high-version (precedence didn't apply)", got.Version)
	}
}

func TestSet_GetMissing(t *testing.T) {
	set, err := skills.LoadAll([]string{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := set.Get("not-a-skill"); !errors.Is(err, skills.ErrNotFound) {
		t.Errorf("expected ErrNotFound; got %v", err)
	}
}

// replaceVersion swaps the version line in the minimalSkill template
// so two writes can produce different versions for the precedence
// test.
func replaceVersion(s, v string) string {
	return strings.Replace(s, "version: 0.1.0", "version: "+v, 1)
}
