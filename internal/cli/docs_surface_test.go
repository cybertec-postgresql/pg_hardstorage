package cli_test

// docs_surface_test.go — every knob an operator can turn must be
// findable in the docs, and every knob the docs describe must exist.
//
// The config file already has this guarantee: configcheck parses the
// YAML in docs/ against the real schema, which is what issue #44
// produced after five pages shipped configuration the loader had never
// supported. The other two operator surfaces had no such guard:
//
//   - repository URL query parameters (?region=, ?endpoint=,
//     ?conditional_put=native, …)
//   - PG_HARDSTORAGE_* environment variables
//
// Both directions matter, and they fail differently. An UNDOCUMENTED
// knob cannot be discovered: `conditional_put=native` gates the
// append-only commit path, and without it an S3-compatible repository
// silently keeps the slower, delete-emitting one — the operator in
// issue #45 could have applied the whole fix and seen nothing change.
// A knob documented but NOT READ is worse: the operator sets it,
// believes it took effect, and it never did. That is exactly what #44
// was.
//
// Harness-only variables are exempt by rule rather than by list: a
// variable read solely from internal/testkit, a _test.go file or the
// doctest harness is not an operator surface. The rule is mechanical,
// so a variable that later reaches production code stops being exempt
// automatically.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var (
	urlParamRe = regexp.MustCompile(`\bq\.Get\("([a-z_]+)"\)`)
	envVarRe   = regexp.MustCompile(`Getenv\("(PG_HARDSTORAGE_[A-Z0-9_]+)"\)`)
)

// isHarnessPath reports whether a file is test/fixture scaffolding
// rather than production code.
func isHarnessPath(rel string) bool {
	switch {
	case strings.HasSuffix(rel, "_test.go"),
		// Any package named testkit, wherever it lives — the first
		// version matched only internal/testkit/ and so flagged
		// internal/pg/testkit's PG_HARDSTORAGE_TEST_PG_MAJOR as an
		// undocumented operator setting.
		strings.Contains(rel, "testkit/"),
		strings.Contains(rel, "internal/verify/sandbox/"),
		strings.Contains(rel, "doctest"):
		return true
	}
	return false
}

// scanSurface walks the tree and returns name → the production files
// that read it.
func scanSurface(t *testing.T, root string, re *regexp.Regexp, onlyUnder string) map[string][]string {
	t.Helper()
	found := map[string][]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			switch info.Name() {
			case "vendor", ".git", "test-runs", "node_modules", "docs":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil || isHarnessPath(rel) {
			return nil
		}
		if onlyUnder != "" && !strings.HasPrefix(rel, onlyUnder) {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		for _, m := range re.FindAllStringSubmatch(string(src), -1) {
			found[m[1]] = append(found[m[1]], rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return found
}

// docsCorpus concatenates every markdown page once.
func docsCorpus(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.Walk(filepath.Join(root, "docs"), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr == nil {
			b.Write(src)
			b.WriteByte('\n')
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk docs: %v", err)
	}
	if b.Len() == 0 {
		t.Fatal("docs corpus is empty; every assertion below would hold vacuously")
	}
	return b.String()
}

// TestStorageURLParamsAreDocumented covers the repository-URL surface.
func TestStorageURLParamsAreDocumented(t *testing.T) {
	root := repoRootFromTest(t)
	params := scanSurface(t, root, urlParamRe, filepath.Join("internal", "plugin", "storage"))
	if len(params) == 0 {
		t.Fatal("found no ?param= reads in the storage plugins — the extraction broke, so " +
			"this test now asserts nothing")
	}
	corpus := docsCorpus(t, root)

	var missing []string
	for name, files := range params {
		if strings.Contains(corpus, "`"+name+"`") || strings.Contains(corpus, name+"=") {
			continue
		}
		sort.Strings(files)
		missing = append(missing, name+"  (read by "+files[0]+")")
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("%d repository-URL parameter(s) are read by the code but appear nowhere in "+
			"docs/:\n  %s\n\nAn operator cannot discover a knob that is not written down — "+
			"and cannot look one up that they find in a colleague's URL. If a parameter is "+
			"deliberately not for general use, document it WITH that warning rather than "+
			"leaving it unfindable.",
			len(missing), strings.Join(missing, "\n  "))
	}
}

// TestOperatorEnvVarsAreDocumented covers the environment surface.
func TestOperatorEnvVarsAreDocumented(t *testing.T) {
	root := repoRootFromTest(t)
	vars := scanSurface(t, root, envVarRe, "")
	if len(vars) == 0 {
		t.Fatal("found no PG_HARDSTORAGE_* reads in production code — extraction broke")
	}
	corpus := docsCorpus(t, root)

	var missing []string
	for name, files := range vars {
		if strings.Contains(corpus, name) {
			continue
		}
		sort.Strings(files)
		missing = append(missing, name+"  (read by "+files[0]+")")
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("%d PG_HARDSTORAGE_* variable(s) are read by production code but appear "+
			"nowhere in docs/:\n  %s\n\nThese are the only channel by which some backends "+
			"can be configured at all — nothing populates StorageConfig.Extras in "+
			"production — so an undocumented one is a setting no operator can find.",
			len(missing), strings.Join(missing, "\n  "))
	}
}

// TestDocumentedStorageParamsExist is the reverse direction, and the
// one that catches issue #44's shape: documentation describing a knob
// the code never reads. The operator sets it, believes it applied, and
// nothing happened.
func TestDocumentedStorageParamsExist(t *testing.T) {
	root := repoRootFromTest(t)
	known := scanSurface(t, root, urlParamRe, filepath.Join("internal", "plugin", "storage"))
	corpus := docsCorpus(t, root)

	// Parameters appearing in a documented repository URL.
	inURL := regexp.MustCompile(`\b(?:s3|gcs|azblob|sftp|scp|file)://[^\s"'` + "`" + `]*`)
	paramInQuery := regexp.MustCompile(`[?&]([a-z_]+)=`)

	// Not every ?x= in a doc URL is ours — PG connection strings and
	// external links carry their own. Only flag parameters on a
	// repository URL scheme.
	seen := map[string]bool{}
	var phantom []string
	for _, u := range inURL.FindAllString(corpus, -1) {
		// Syntax templates carry metasyntactic placeholders —
		// `azblob://<account>/<container>[?option=value&...]` — which
		// are not parameters anyone can set. Flagging them is noise
		// that trains a reader to ignore this test.
		if strings.ContainsAny(u, "<>[]") {
			continue
		}
		for _, m := range paramInQuery.FindAllStringSubmatch(u, -1) {
			name := m[1]
			if seen[name] || len(known[name]) > 0 {
				continue
			}
			seen[name] = true
			phantom = append(phantom, name+"  (in a documented URL: "+truncURL(u)+")")
		}
	}
	if len(phantom) > 0 {
		sort.Strings(phantom)
		t.Errorf("%d parameter(s) appear in documented repository URLs but no storage "+
			"plugin reads them:\n  %s\n\nAn operator following the docs sets these and "+
			"believes they took effect. Nothing did. That is what issue #44 was — five "+
			"pages describing configuration the loader had never supported.",
			len(phantom), strings.Join(phantom, "\n  "))
	}
}

func truncURL(u string) string {
	if len(u) > 72 {
		return u[:72] + "…"
	}
	return u
}
