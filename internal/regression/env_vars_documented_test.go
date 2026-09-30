package regression

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestDocumentedEnvVarsExist: every PG_HARDSTORAGE_* variable the docs
// tell someone to set must be read somewhere — product code, or tests
// for the test-harness variables.
//
// Three shipped with nothing behind them, each failing silently:
//
//   - PG_HARDSTORAGE_REPO — "same as --repo, via env var". For a
//     configured deployment the backup then went to the CONFIGURED
//     repository, not the emergency one the operator named.
//   - PG_HARDSTORAGE_OTLP_ENDPOINT / _INSECURE — no tracing, no error.
//   - PG_HARDSTORAGE_KEYRING_PASSPHRASE — set by the Helm guide, read by
//     nothing.
func TestDocumentedEnvVarsExist(t *testing.T) {
	root := repoRoot(t)
	envRe := regexp.MustCompile(`PG_HARDSTORAGE_[A-Z0-9_]*[A-Z0-9]`)
	goLiteralEnvRe := regexp.MustCompile(`"(PG_HARDSTORAGE_[A-Z0-9_]*[A-Z0-9])"`)

	read := map[string]bool{}
	for _, dir := range []string{"cmd", "internal", "compat"} {
		_ = filepath.WalkDir(filepath.Join(root, dir), func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.Contains(p, "_mutation_") {
				return err
			}
			b, _ := os.ReadFile(p)
			// Only quoted literals count: that is how Go reads an env
			// var (os.Getenv("...")), and it keeps comments — this
			// file's own list of offenders included — from counting.
			for _, m := range goLiteralEnvRe.FindAllStringSubmatch(string(b), -1) {
				read[m[1]] = true
			}
			return nil
		})
	}
	for _, f := range []string{"Makefile", "run_testing.sh", "run_compat_testing.sh"} {
		if b, err := os.ReadFile(filepath.Join(root, f)); err == nil {
			for _, v := range envRe.FindAllString(string(b), -1) {
				read[v] = true
			}
		}
	}

	// A variable, not a marker: `__PG_HARDSTORAGE_VERIFY__` is a
	// console sentinel the Firecracker rootfs prints, not an env var.
	docEnvRe := regexp.MustCompile(`(?:^|[^_A-Za-z0-9])(PG_HARDSTORAGE_[A-Z0-9_]*[A-Z0-9])(?:[^_A-Za-z0-9]|$)`)
	dup := map[string]bool{}
	var missing []string
	_ = filepath.WalkDir(filepath.Join(root, "docs"), func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".md") {
			return err
		}
		// The changelog is history: it names removed variables on
		// purpose, to say they were removed. It instructs no one.
		if filepath.Base(p) == "changelog.md" {
			return nil
		}
		b, _ := os.ReadFile(p)
		for _, m := range docEnvRe.FindAllStringSubmatch(string(b), -1) {
			if v := m[1]; !read[v] {
				rel, _ := filepath.Rel(root, p)
				seen := v + " (" + rel + ")"
				if !dup[seen] {
					dup[seen] = true
					missing = append(missing, seen)
				}
			}
		}
		return nil
	})
	sort.Strings(missing)
	for _, m := range missing {
		t.Errorf("documented but read by nothing: %s", m)
	}
}
