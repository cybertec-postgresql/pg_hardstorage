package cli_test

// docs_kekref_scheme_test.go — meta-test pinning that every KEKRef
// scheme the documentation teaches is a scheme this binary actually
// claims.
//
// Sibling of docs_cli_reachability_test.go (CLI verbs) and
// internal/config/configcheck's docs YAML check (config keys). This one
// covers the third way a page can be confidently wrong: a real key with
// a plausible-but-nonexistent scheme.
//
// The motivating instance: the encryption tutorial and `kms --help`
// both advertised `azure-key-vault://`. The provider registers
// `azure-kv` (internal/plugin/kms/azurekv: Scheme = "azure-kv"), so an
// operator following the tutorial got
// `kms: unknown KEKRef scheme` on their first encrypted backup.
//
// Scope is deliberately narrow to stay false-positive-free: only
// scheme tokens that appear AS a KEKRef are checked — the value of a
// `kek_ref:` YAML key, or the argument to `--kek` / `--old-kek-ref` /
// `--new-kek-ref`. Storage URLs (s3://, azblob://, file://) and prose
// links are never inspected.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/kms"
)

// kekRefValue matches a KEKRef in the two shapes docs use it:
//
//	kek_ref: aws-kms://alias/prod        (YAML, optionally quoted)
//	--kek aws-kms://alias/prod           (CLI flag, also --old/new-kek-ref)
var kekRefValue = regexp.MustCompile(
	`(?:kek_ref:\s*|--(?:old-|new-)?kek(?:-ref)?[ =])['"]?([a-z0-9][a-z0-9+.-]*)://`)

func TestDocsKEKRefSchemesAreRegistered(t *testing.T) {
	known := map[string]bool{}
	for _, s := range kms.DefaultRegistry.Schemes() {
		known[s] = true
	}
	if len(known) == 0 {
		t.Fatal("no KMS schemes registered — the plugin init() wiring is missing, test would pass vacuously")
	}
	// The keystore resolves local: itself; it never reaches the registry.
	known["local"] = true
	// pkcs11 is gated behind a build tag. The docs must be free to
	// document it from any build flavour, so accept it unconditionally
	// rather than failing every default-build test run.
	known["pkcs11"] = true

	root := docsRoot(t)
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
		t.Fatal("no markdown under docs/")
	}

	seen := map[string]bool{}
	checked := 0
	for _, path := range files {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		rel, _ := filepath.Rel(root, path)
		for _, m := range kekRefValue.FindAllStringSubmatch(string(body), -1) {
			scheme := m[1]
			checked++
			if known[scheme] {
				seen[scheme] = true
				continue
			}
			t.Errorf("docs/%s: KEKRef scheme %q://… is not registered by any KMS provider — "+
				"an operator following this page gets `kms: unknown KEKRef scheme` on their first "+
				"encrypted backup. Registered: %s",
				filepath.ToSlash(rel), scheme, strings.Join(sortedKeys(known), ", "))
		}
	}
	if checked == 0 {
		t.Fatal("no KEKRef values found in docs/ — the regex stopped matching; the test is no longer checking anything")
	}
	t.Logf("checked %d KEKRef occurrences across %d files; schemes seen: %s",
		checked, len(files), strings.Join(sortedKeys(seen), ", "))
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
