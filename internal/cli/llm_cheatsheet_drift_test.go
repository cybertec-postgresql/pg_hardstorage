package cli

import (
	"regexp"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/cli/cmdtree"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/chat"
)

// TestFlagCheatsheet_NoDrift parses the FlagCheatsheet block and
// asserts every clearly-marked negative flag claim is still
// accurate against the live cobra tree.
//
// The cheatsheet structure is fuzzy English, so this test is
// conservative — it ONLY validates "No `--<flag>`" (and "no such
// flag") forms.  Positive flag claims aren't validated here
// because the cheatsheet sometimes lists flags in
// non-target-command context (e.g. "these belong to `rotate`, NOT
// `schedule`"), and parsing fuzzy English to disambiguate
// produces too many false positives.
//
// What this catches:
//   - Anyone adding `--recover` to `repair scrub` while the
//     cheatsheet still says "No `--recover`" — the model would
//     get told the flag doesn't exist when it does.
//
// What this does NOT catch:
//   - Anyone renaming `--heal` to something else.  That kind of
//     drift would need fully structured cheatsheet metadata
//     (YAML with per-flag entries) instead of prose; out of
//     scope for this guard.
//
// The pre-loaded `--help` blocks in the system prompt cover the
// rename case categorically since they're auto-rendered from
// cmdtree.Help() at session bootstrap.
func TestFlagCheatsheet_NoDrift(t *testing.T) {
	root := NewRoot()
	tree := cmdtree.Walk(root)
	if tree == nil {
		t.Fatal("could not walk cobra tree")
	}

	text := chat.FlagCheatsheet()
	if text == "" {
		t.Fatal("FlagCheatsheet() returned empty")
	}

	// Split into bullets — each bullet starts with "- `<path>` —"
	// at the start of a line.  Compound bullets like
	// "- `partial inspect` / `partial restore` — ..." apply the
	// negative claims to BOTH paths.
	bulletRE := regexp.MustCompile("(?m)^- `([^`]+)`(?:\\s*/\\s*`([^`]+)`)?\\s+— ")
	matches := bulletRE.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		t.Fatal("found no bullets in cheatsheet — parser broken?")
	}

	// Negation pattern: "no `--name`" / "No `--name`" / "NOT `--name`".
	// Captures the flag name (group 1).
	negRE := regexp.MustCompile("(?i)\\b(?:no|not)\\s+`--([a-z][a-z0-9-]+)`")

	for i, m := range matches {
		paths := []string{text[m[2]:m[3]]}
		if m[4] != -1 {
			paths = append(paths, text[m[4]:m[5]])
		}

		// Body: from the end of this bullet's header to the
		// start of the next bullet.
		start := m[1]
		end := len(text)
		if i+1 < len(matches) {
			end = matches[i+1][0]
		}
		body := text[start:end]

		// Pull every "no --flag" / "NOT --flag" claim.
		for _, neg := range negRE.FindAllStringSubmatch(body, -1) {
			flagName := neg[1]
			for _, path := range paths {
				node := tree.Find(strings.Fields(path))
				if node == nil {
					t.Errorf("cheatsheet references command %q which is not in the cobra tree", path)
					continue
				}
				if flagExists(node, flagName) {
					t.Errorf("DRIFT: cheatsheet for %q says --%s does NOT exist, but cobra tree now has it — remove or update the negative claim",
						path, flagName)
				}
			}
		}
	}
}

func flagExists(node *cmdtree.Node, name string) bool {
	for _, f := range node.Flags {
		if f.Name == name {
			return true
		}
	}
	return false
}
