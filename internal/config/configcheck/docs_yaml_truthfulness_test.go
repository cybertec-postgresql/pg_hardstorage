package configcheck_test

// docs_yaml_truthfulness_test.go — meta-test pinning that every
// pg_hardstorage config snippet in docs/ parses against the REAL
// config schema.
//
// Sibling of internal/cli/docs_cli_reachability_test.go, which does the
// same job for CLI invocations. Issue #44 is the config-side instance
// of that failure class: five KMS how-tos documented a `kms:` block and
// a per-deployment `kek_ref` that the strict loader rejected outright,
// so an operator who followed the page verbatim got
// `field kms not found in type config.Config` from `lint`. cmd/doctest
// only executes bash blocks, so nothing looked at the YAML.
//
// If this test fails, EITHER:
//
//	(a) the docs invented a key → fix the page, OR
//	(b) the schema dropped/renamed a key the docs still teach →
//	    restore it or update every page that uses it.

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/config/configcheck"
)

// docsRoot finds the repo's docs/ dir relative to this test file so
// the test works from any cwd.
func docsRoot(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// internal/config/configcheck → ../../../docs
	return filepath.Clean(filepath.Join(filepath.Dir(here), "..", "..", "..", "docs"))
}

// knownDrift exempts pages whose config snippets are known-wrong and
// not yet fixed. It is EMPTY, and should stay that way.
//
// It briefly held three families this test found on the day it was
// written. All three turned out to be the same defect as issue #44 —
// not "a key that does nothing", but a key that makes the whole file
// fail to load, because the loader is strict (KnownFields). Any
// operator who copied those pages got a config the binary refused:
//
//   - sinks[].filter (7 sink how-tos + operator-guide) — plugins read
//     min_severity from `config:`; `components` filtering was never
//     implemented by any sink.
//   - deployments[].extras (repository-scp / -sftp) — no such schema
//     field, and nothing populates the storage plugin's Extras map in
//     production either, so scp:// cannot authenticate at all.
//   - deployments[].worm / worm_retention (pci-dss.md) — WORM is a
//     repo-init property (`repo init --worm-mode`), deliberately not
//     per-deployment.
//
// The lesson worth keeping: a "docs-only" finding here is worth
// running against the real binary before believing it is cosmetic.
//
// Adding an entry is a deliberate act. An entry that stops being
// needed FAILS the test, so a fix cannot leave a stale exemption.
var knownDrift = map[string][]string{}

func TestDocsConfigSnippetsMatchSchema(t *testing.T) {
	root := docsRoot(t)
	if _, err := os.Stat(root); err != nil {
		t.Skipf("docs/ not present: %v", err)
	}

	var files []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".md") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk docs: %v", err)
	}
	sort.Strings(files)
	if len(files) == 0 {
		t.Fatal("no markdown found under docs/ — the test would pass vacuously")
	}

	scanned := 0
	unused := map[string]map[string]bool{}
	for path, keys := range knownDrift {
		unused[path] = map[string]bool{}
		for _, k := range keys {
			unused[path][k] = true
		}
	}

	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		findings := configcheck.Scrub(string(body))
		scanned++
		rel, _ := filepath.Rel(root, f)
		rel = filepath.ToSlash(rel)
		for _, fi := range findings {
			if unused[rel][fi.Key] {
				delete(unused[rel], fi.Key)
				continue
			}
			if _, exempt := knownDrift[rel]; exempt && !unused[rel][fi.Key] {
				// Already accounted for by an earlier block in the
				// same page.
				if containsKey(knownDrift[rel], fi.Key) {
					continue
				}
			}
			where := fi.Path
			if where == "" {
				where = "(root)"
			}
			msg := fi.Message
			if fi.Suggestion != "" {
				msg = "did you mean " + fi.Suggestion + "?"
			}
			t.Errorf("docs/%s: config snippet has %s %q under %s — %s",
				rel, fi.Kind, fi.Key, where, msg)
		}
	}

	for path, keys := range unused {
		for k := range keys {
			t.Errorf("knownDrift[%q] still lists %q, but the docs no longer use it — "+
				"delete the exemption so it can't mask a future regression", path, k)
		}
	}
	t.Logf("scanned %d markdown files under docs/", scanned)
}

func containsKey(keys []string, k string) bool {
	for _, v := range keys {
		if v == k {
			return true
		}
	}
	return false
}
