package docs_test

// corpus_freshness_test.go — pins that the go:embed corpus under
// internal/llm/docs/ matches the canonical docs it is copied from.
//
// go:embed cannot reach outside its own package directory, so
// `make sync-llm-docs` copies docs/reference/runbooks/*.md, CHANGELOG.md
// and README.md in. Nothing enforced that the copy was ever re-run: the
// Makefile comment claimed "CI checks that the bundled copies match",
// but no workflow did.
//
// The cost of that gap is not cosmetic. This corpus is what the LLM
// assistant serves an operator mid-incident. When the canonical
// runbooks were corrected to stop advertising flags the binary doesn't
// have, the embedded copies kept the old text — so R2 still told
// operators to run `audit append --type kms.shred --kek-ref …`, three
// flags that no longer parse, at the exact moment they're recovering
// from a destroyed KMS key.
//
// If this test fails, run `make sync-llm-docs` and commit the result.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// repoRoot locates the checkout root relative to this file so the test
// works from any cwd.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// internal/llm/docs → ../../..
	return filepath.Clean(filepath.Join(filepath.Dir(here), "..", "..", ".."))
}

func TestBundledCorpusMatchesCanonicalDocs(t *testing.T) {
	root := repoRoot(t)
	bundled := filepath.Join(root, "internal", "llm", "docs")

	// Mirrors the cp commands in the Makefile's sync-llm-docs target.
	// canonical → bundled.
	pairs := map[string]string{
		filepath.Join(root, "CHANGELOG.md"): filepath.Join(bundled, "root", "CHANGELOG.md"),
		filepath.Join(root, "README.md"):    filepath.Join(bundled, "root", "README.md"),
	}

	runbookDir := filepath.Join(root, "docs", "reference", "runbooks")
	entries, err := os.ReadDir(runbookDir)
	if err != nil {
		t.Fatalf("read canonical runbooks: %v", err)
	}
	canonicalRunbooks := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		canonicalRunbooks[e.Name()] = true
		pairs[filepath.Join(runbookDir, e.Name())] = filepath.Join(bundled, "runbooks", e.Name())
	}
	if len(canonicalRunbooks) == 0 {
		t.Fatal("no canonical runbooks found — the test would pass vacuously")
	}

	for canonical, embedded := range pairs {
		rel, _ := filepath.Rel(root, canonical)
		want, err := os.ReadFile(canonical)
		if err != nil {
			t.Errorf("read %s: %v", rel, err)
			continue
		}
		got, err := os.ReadFile(embedded)
		if err != nil {
			t.Errorf("%s has no bundled copy (%v) — run `make sync-llm-docs`", rel, err)
			continue
		}
		if string(got) != string(want) {
			t.Errorf("bundled copy of %s is stale — run `make sync-llm-docs` and commit.\n"+
				"The LLM assistant serves this text to operators during incidents, so a stale "+
				"copy hands them commands that no longer work.", rel)
		}
	}

	// A runbook deleted upstream but left behind here would still be
	// searchable by the assistant, and would still be quoted as current.
	bundledEntries, err := os.ReadDir(filepath.Join(bundled, "runbooks"))
	if err != nil {
		t.Fatalf("read bundled runbooks: %v", err)
	}
	for _, e := range bundledEntries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		if !canonicalRunbooks[e.Name()] {
			t.Errorf("bundled runbook %q no longer exists under docs/reference/runbooks/ — "+
				"delete it from internal/llm/docs/runbooks/", e.Name())
		}
	}
}

// TestSyncRecipeIsFullyCovered keeps the check above honest about its
// own inputs.
//
// TestBundledCorpusMatchesCanonicalDocs hardcodes the file list it
// compares — CHANGELOG.md, README.md, and the runbooks directory. That
// list was transcribed from `make sync-llm-docs` by hand, so adding a
// fourth `cp` to the recipe would start shipping a file that nothing
// ever checks for staleness. The corpus is what the assistant serves
// operators mid-incident; a silently-unchecked file in it is exactly
// the failure this suite exists to prevent.
//
// So: parse the recipe, and require every destination it copies into to
// be one the freshness test actually covers.
func TestSyncRecipeIsFullyCovered(t *testing.T) {
	root := repoRoot(t)
	mk, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}

	// Pull the sync-llm-docs recipe body.
	lines := strings.Split(string(mk), "\n")
	var recipe []string
	inRecipe := false
	for _, ln := range lines {
		if strings.HasPrefix(ln, "sync-llm-docs:") {
			inRecipe = true
			continue
		}
		if inRecipe {
			if !strings.HasPrefix(ln, "\t") {
				break
			}
			recipe = append(recipe, strings.TrimSpace(strings.TrimPrefix(ln, "\t")))
		}
	}
	if len(recipe) == 0 {
		t.Fatal("could not find the sync-llm-docs recipe in the Makefile — " +
			"this test is no longer reading it")
	}

	// Destinations the freshness test knows how to verify.
	covered := map[string]bool{
		"internal/llm/docs/runbooks/": true,
		"internal/llm/docs/root/":     true,
	}

	copies := 0
	for _, cmd := range recipe {
		cmd = strings.TrimPrefix(cmd, "@")
		if !strings.HasPrefix(cmd, "cp ") {
			continue
		}
		fields := strings.Fields(cmd)
		if len(fields) < 3 {
			continue
		}
		copies++
		dst := fields[len(fields)-1]
		if !covered[dst] {
			t.Errorf("sync-llm-docs copies into %q, which "+
				"TestBundledCorpusMatchesCanonicalDocs does not verify — that file "+
				"would ship in the embedded corpus with no staleness check. Extend "+
				"the pairs map there, then add the destination here.", dst)
		}
	}
	if copies == 0 {
		t.Fatal("the sync-llm-docs recipe contains no cp commands — either the " +
			"recipe changed shape or the parse broke")
	}
	t.Logf("sync recipe: %d cp command(s), all covered", copies)
}
