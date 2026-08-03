package skills

// schema_doc_truthfulness_test.go — the documented skill schema must
// match the struct that actually parses skill files.
//
// docs/reference/skill-schema.md is what an operator writes a skill
// against. Parse decodes with KnownFields=true, so a key the doc
// invents but the struct lacks is not a cosmetic error: the operator's
// file is REJECTED OUTRIGHT, with a message naming a field they took
// straight from the reference page. That is the exact shape of issue
// #44 — documented config the strict loader refused — and it has since
// been found in five other places on this project.
//
// The inverse matters too: a struct field with no documentation is
// surface nobody can discover.
//
// Existing coverage stops short of this. TestLoadBuiltins_AllAreLintClean
// proves the four SHIPPED skills parse, which says nothing about
// whether the page describes them correctly — a doc can be wrong about
// keys no builtin happens to use.

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// docKeyRow matches a row of the "Top-level keys" table:
//
//	| `name` | string | yes | Stable machine name |
var docKeyRow = regexp.MustCompile(`^\|\s*\x60([a-z_][a-z0-9_]*)\x60\s*\|`)

func skillSchemaDoc(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// internal/llm/skills → repo root
	root := filepath.Clean(filepath.Join(filepath.Dir(here), "..", "..", ".."))
	p := filepath.Join(root, "docs", "reference", "skill-schema.md")
	body, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read skill-schema.md: %v", err)
	}
	return string(body)
}

// documentedTopLevelKeys extracts the keys listed in the "Top-level
// keys" section only — later sections document nested mappings
// (`trigger`, `permissions`, …) whose keys live on other structs.
func documentedTopLevelKeys(t *testing.T, doc string) map[string]bool {
	t.Helper()
	lines := strings.Split(doc, "\n")
	keys := map[string]bool{}
	inSection := false
	for _, ln := range lines {
		if strings.HasPrefix(ln, "## ") {
			inSection = strings.TrimSpace(ln) == "## Top-level keys"
			continue
		}
		if !inSection {
			continue
		}
		if m := docKeyRow.FindStringSubmatch(ln); m != nil {
			keys[m[1]] = true
		}
	}
	return keys
}

// structTopLevelKeys reads the yaml tags off Skill. Fields tagged "-"
// are internal bookkeeping (Source) and are not part of the file
// format.
func structTopLevelKeys() map[string]bool {
	keys := map[string]bool{}
	rt := reflect.TypeOf(Skill{})
	for i := 0; i < rt.NumField(); i++ {
		tag := rt.Field(i).Tag.Get("yaml")
		if tag == "" || tag == "-" {
			continue
		}
		name := strings.TrimSpace(strings.Split(tag, ",")[0])
		if name == "" || name == "-" {
			continue
		}
		keys[name] = true
	}
	return keys
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestSkillSchemaDocMatchesStruct(t *testing.T) {
	doc := skillSchemaDoc(t)
	documented := documentedTopLevelKeys(t, doc)
	actual := structTopLevelKeys()

	if len(documented) == 0 {
		t.Fatal("parsed zero keys from the 'Top-level keys' table — the doc's shape " +
			"changed and this test is no longer reading it")
	}
	if len(actual) == 0 {
		t.Fatal("Skill has no yaml-tagged fields — reflection is not finding the struct")
	}

	for _, k := range sortedSet(documented) {
		if !actual[k] {
			t.Errorf("skill-schema.md documents top-level key %q, which Skill does not "+
				"accept — Parse uses KnownFields=true, so an operator who copies this "+
				"key from the reference page gets their whole skill file rejected", k)
		}
	}
	for _, k := range sortedSet(actual) {
		if !documented[k] {
			t.Errorf("Skill accepts top-level key %q, which skill-schema.md does not "+
				"document — undiscoverable surface", k)
		}
	}
	t.Logf("skill schema: %d documented keys, %d struct keys", len(documented), len(actual))
}

// TestSkillSchemaDoc_VersionStringMatches pins the schema constant the
// page quotes. A bumped constant with a stale page sends every operator
// writing a skill against a version the loader rejects.
func TestSkillSchemaDoc_VersionStringMatches(t *testing.T) {
	doc := skillSchemaDoc(t)
	if !strings.Contains(doc, SchemaSkill) {
		t.Errorf("skill-schema.md never mentions the current schema string %q — "+
			"the page tells operators to write a version the loader will refuse",
			SchemaSkill)
	}
}
