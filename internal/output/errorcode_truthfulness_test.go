package output_test

// errorcode_truthfulness_test.go — every error namespace the binary can
// emit must appear in docs/reference/error-codes.md.
//
// Sibling of exitcode_truthfulness_test.go, which runs the OTHER
// direction: it takes the documented namespace→exit rows and checks the
// dispatcher routes them that way. Nothing walked code→doc, so a new
// namespace could ship with no documentation at all and both the exit
// tests and the docs build would stay green.
//
// That is not hypothetical. When this test was written it found two
// live namespaces with no entry on the page:
//
//	manifest.*  — manifest.invalid, raised by the restore planner
//	demo.*      — five codes from `pg_hardstorage demo` bring-up
//
// The page's own contract is namespace-level, not leaf-level: it says
// outright that individual leaf codes are not exhaustively listed but
// that "every leaf belongs to one of the namespaces below". So this
// asserts exactly that claim and no more — requiring every one of the
// ~475 leaves to be listed would be inventing a promise the docs never
// made, and would rot instantly.
//
// Scope note: `output.NewError("literal", …)` AND
// `fmt.Errorf("ns.leaf: …")` are scanned. The second form was a blind
// spot with real consequences — the whole `splitbrain.*` namespace
// (raised by wal push when another writer already archived a segment)
// is built with fmt.Errorf, so it shipped undocumented and this guard
// reported nothing. An operator hitting splitbrain.content_mismatch,
// which is the archive refusing a divergent writer, had nowhere to look
// it up.
//
// Codes built from a variable still cannot be resolved without
// type-checking the whole tree, and there are none today; if that
// changes the scan under-reports rather than failing loudly, so the
// count assertion at the end is the tripwire.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func repoRootFromOutput(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// internal/output → repo root
	return filepath.Clean(filepath.Join(filepath.Dir(here), "..", ".."))
}

// documentedNamespaces parses the namespaces the page names, in either
// shape it uses: a wildcard row (`backup.*`), a bare namespace row
// (`internal`), or an inline leaf example (`config.invalid`).
func documentedNamespaces(t *testing.T, root string) map[string]bool {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, "docs", "reference", "error-codes.md"))
	if err != nil {
		t.Fatalf("read error-codes.md: %v", err)
	}
	doc := string(body)
	ns := map[string]bool{}

	// `foo.*`
	for _, m := range regexp.MustCompile(`\x60([a-z_][a-z0-9_]*)\.\*\x60`).FindAllStringSubmatch(doc, -1) {
		ns[m[1]] = true
	}
	// `foo.bar` — an inline leaf example documents its namespace too
	for _, m := range regexp.MustCompile(`\x60([a-z_][a-z0-9_]*)\.[a-z0-9_.]+\x60`).FindAllStringSubmatch(doc, -1) {
		ns[m[1]] = true
	}
	// A bare `| \x60foo\x60 |` table cell, e.g. the `internal` catch-all.
	for _, m := range regexp.MustCompile(`\|\s*\x60([a-z_][a-z0-9_]*)\x60\s*\|`).FindAllStringSubmatch(doc, -1) {
		ns[m[1]] = true
	}
	return ns
}

// emittedCodes walks the production tree for output.NewError string
// literals and returns code → one example file:line.
func emittedCodes(t *testing.T, root string) map[string]string {
	t.Helper()
	codes := map[string]string{}
	fset := token.NewFileSet()

	for _, top := range []string{"internal", "cmd"} {
		base := filepath.Join(root, top)
		if _, err := os.Stat(base); err != nil {
			continue
		}
		err := filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
			if err != nil || !d.IsDir() {
				return err
			}
			pkgs, perr := parser.ParseDir(fset, path, func(fi fs.FileInfo) bool {
				return !strings.HasSuffix(fi.Name(), "_test.go")
			}, 0)
			if perr != nil {
				return nil // not a package dir; other tests cover parse health
			}
			for _, pkg := range pkgs {
				for fpath, file := range pkg.Files {
					ast.Inspect(file, func(n ast.Node) bool {
						call, ok := n.(*ast.CallExpr)
						if !ok || len(call.Args) == 0 {
							return true
						}
						sel, ok := call.Fun.(*ast.SelectorExpr)
						if !ok || sel.Sel.Name != "NewError" {
							return true
						}
						if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "output" {
							return true
						}
						lit, ok := call.Args[0].(*ast.BasicLit)
						if !ok || lit.Kind != token.STRING {
							return true
						}
						code, uerr := strconv.Unquote(lit.Value)
						if uerr != nil || code == "" {
							return true
						}
						if _, seen := codes[code]; !seen {
							rel, _ := filepath.Rel(root, fpath)
							pos := fset.Position(lit.Pos())
							codes[code] = filepath.ToSlash(rel) + ":" + strconv.Itoa(pos.Line)
						}
						return true
					})

					// Second pass: fmt.Errorf("ns.leaf: ...") — the
					// shape splitbrain.* uses. Anchored at the start of
					// the format string and requiring the trailing
					// colon, so ordinary prose ("patroni: build %s")
					// does not match.
					for _, m := range errorfCodeRe.FindAllStringSubmatch(readFile(fpath), -1) {
						if _, seen := codes[m[1]]; seen {
							continue
						}
						rel, _ := filepath.Rel(root, fpath)
						codes[m[1]] = filepath.ToSlash(rel)
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", base, err)
		}
	}
	return codes
}

// errorfCodeRe matches a structured code raised through fmt.Errorf.
var errorfCodeRe = regexp.MustCompile(`fmt\.Errorf\(\s*"([a-z_][a-z0-9_]*\.[a-z0-9_]+):`)

// readFile is a swallow-errors helper: a file that cannot be read is
// simply not scanned, matching the AST pass's posture.
func readFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

func TestErrorCodes_EveryEmittedNamespaceIsDocumented(t *testing.T) {
	root := repoRootFromOutput(t)
	documented := documentedNamespaces(t, root)
	if len(documented) == 0 {
		t.Fatal("parsed zero namespaces from error-codes.md — the doc shape changed " +
			"and this test is no longer reading it")
	}
	codes := emittedCodes(t, root)
	if len(codes) < 100 {
		t.Fatalf("found only %d error codes in the tree — the AST scan stopped matching, "+
			"so this test is no longer checking anything", len(codes))
	}

	type miss struct{ ns, code, where string }
	var missing []miss
	for code, where := range codes {
		ns := code
		if i := strings.Index(code, "."); i > 0 {
			ns = code[:i]
		}
		if documented[ns] {
			continue
		}
		missing = append(missing, miss{ns, code, where})
	}
	sort.Slice(missing, func(i, j int) bool { return missing[i].code < missing[j].code })

	seenNS := map[string]bool{}
	for _, m := range missing {
		if seenNS[m.ns] {
			continue // one complaint per namespace is enough to act on
		}
		seenNS[m.ns] = true
		t.Errorf("error namespace %q is emitted but not documented — e.g. %q at %s. "+
			"Add a row to docs/reference/error-codes.md; an operator hitting this code "+
			"has nowhere to look up what it means or how to recover.",
			m.ns, m.code, m.where)
	}
	t.Logf("checked %d distinct error codes against %d documented namespaces",
		len(codes), len(documented))
}
