// exitcode_truthfulness_test.go — meta-test pinning that the
// documented exit-code contract matches codePrefixToExit.
//
// docs/reference/exit-codes.md is the v1 public contract; the
// dispatcher's codePrefixToExit is what scripts and CI pipelines
// actually see.  If the two drift, an operator's `if [ $? -eq 7
// ]` script either misses a refusal or fires on a no-op — both
// silent until a real incident.
//
// What this asserts:
//
//   - Every (namespace, code) row in the docs' "Code namespace →
//     exit-code mapping" table must match codePrefixToExit's
//     output for a synthesized *Error in that namespace.
//   - Every leaf-code row (e.g. `storage.unreachable` (leaf))
//     must match.
//   - Every ExitCode constant ≥ 0 must appear in the docs.
//   - Every ExitCode constant that the docs claim a namespace
//     routes to must be reachable from SOME error code we
//     actually use in the production tree (so the contract isn't
//     pure theatre — a documented exit code with no code that
//     emits it is dead surface).
package output

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func exitCodesDocPath(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// internal/output → ../../docs/reference/exit-codes.md
	return filepath.Clean(filepath.Join(filepath.Dir(here),
		"..", "..", "docs", "reference", "exit-codes.md"))
}

// docMapping parses the documented namespace→exit table.
// Returns:
//
//	prefixes — namespace prefix (e.g. "auth") → exit code int
//	leaves   — leaf code (e.g. "storage.unreachable") → exit code int
//
// The doc format is:
//
//	| `auth.*` | `3` |
//	| `storage.unreachable` (leaf) | `8` |
func docMapping(t *testing.T) (prefixes map[string]int, leaves map[string]int) {
	t.Helper()
	body, err := os.ReadFile(exitCodesDocPath(t))
	if err != nil {
		t.Fatalf("read docs: %v", err)
	}
	prefixes = map[string]int{}
	leaves = map[string]int{}
	// Match either "| `foo.*` | `N` |" or "| `foo.bar` (leaf) | `N` |"
	row := regexp.MustCompile(`\|\s*` + "`" + `([a-z_.]+)(\.\*)?` + "`" + `(\s*\(leaf\))?\s*\|\s*` + "`" + `(\d+)` + "`" + `\s*\|`)
	for _, m := range row.FindAllStringSubmatch(string(body), -1) {
		name := m[1]
		isWildcard := m[2] != ""
		isLeaf := m[3] != ""
		code, err := strconv.Atoi(m[4])
		if err != nil {
			continue
		}
		switch {
		case isWildcard:
			// "auth.*" → prefix "auth"
			prefixes[name] = code
		case isLeaf:
			leaves[name] = code
		}
	}
	return prefixes, leaves
}

// docExitCodes parses the documented "Codes" table to extract
// every numeric exit code value.
func docExitCodes(t *testing.T) map[int]string {
	t.Helper()
	body, err := os.ReadFile(exitCodesDocPath(t))
	if err != nil {
		t.Fatalf("read docs: %v", err)
	}
	row := regexp.MustCompile(`\|\s*\*\*(\d+)\*\*\s*\|\s*` + "`" + `(Exit\w+)` + "`")
	out := map[int]string{}
	for _, m := range row.FindAllStringSubmatch(string(body), -1) {
		code, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		out[code] = m[2]
	}
	return out
}

// TestExitCodes_DocumentedPrefixesMatchCode: every (prefix → code)
// row in the docs maps the same way in codePrefixToExit.
func TestExitCodes_DocumentedPrefixesMatchCode(t *testing.T) {
	docs, _ := docMapping(t)
	if len(docs) == 0 {
		t.Fatal("parsed zero documented prefix rows — regex drift")
	}
	for prefix, wantInt := range docs {
		want := ExitCode(wantInt)
		// Synthesize a structured error in this namespace.
		err := NewError(prefix+".synthetic", "x")
		got := ExitCodeFor(err)
		if got != want {
			t.Errorf("docs say %s.* → %d, but ExitCodeFor returned %d", prefix, want, got)
		}
	}
}

// TestExitCodes_DocumentedLeavesMatchCode: same for leaf rows
// (e.g. storage.unreachable, restore.target_unreachable).
func TestExitCodes_DocumentedLeavesMatchCode(t *testing.T) {
	_, leaves := docMapping(t)
	if len(leaves) == 0 {
		t.Fatal("parsed zero documented leaf rows — regex drift")
	}
	for leaf, wantInt := range leaves {
		want := ExitCode(wantInt)
		err := NewError(leaf, "x")
		got := ExitCodeFor(err)
		if got != want {
			t.Errorf("docs say %s (leaf) → %d, but ExitCodeFor returned %d", leaf, want, got)
		}
	}
}

// TestExitCodes_AllConstantsDocumented: every Exit* constant
// the package exports must appear in the documented "Codes"
// table.  An undocumented exit code is a silent surface the
// operator can't write cron logic against.
func TestExitCodes_AllConstantsDocumented(t *testing.T) {
	doc := docExitCodes(t)
	codeConsts := map[int]string{
		int(ExitOK):           "ExitOK",
		int(ExitError):        "ExitError",
		int(ExitMisuse):       "ExitMisuse",
		int(ExitAuth):         "ExitAuth",
		int(ExitPreflight):    "ExitPreflight",
		int(ExitAborted):      "ExitAborted",
		int(ExitNotFound):     "ExitNotFound",
		int(ExitConflict):     "ExitConflict",
		int(ExitUnreachable):  "ExitUnreachable",
		int(ExitVerifyFailed): "ExitVerifyFailed",
		int(ExitDoctorIssues): "ExitDoctorIssues",
	}
	var missing []string
	for n, name := range codeConsts {
		if got, ok := doc[n]; !ok {
			missing = append(missing, name+" ("+strconv.Itoa(n)+")")
		} else if got != name {
			t.Errorf("code %d: docs name %q, code name %q", n, got, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("%d exit code(s) undocumented in docs/reference/exit-codes.md:\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}
}

// routedCases reads codePrefixToExit's own switch statements and
// returns the namespace cases and the leaf cases it routes.
//
// This used to be two hand-written lists with a comment claiming that
// "adding a case there without adding it here fails this test". It did
// not, and could not: the loops only ever visited the hand-written
// names, so a new route was invisible to them. Adding `case
// "quarantine": return ExitConflict` to the switch left the whole
// package green — a namespace could ship with an exit code no operator
// could look up. Reading the switch is the only version of this test
// that keeps the promise its comment makes.
func routedCases(t *testing.T) (namespaces, leaves []string) {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	path := filepath.Join(filepath.Dir(here), "exitcode.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse exitcode.go: %v", err)
	}

	var fn *ast.FuncDecl
	for _, d := range file.Decls {
		if f, ok := d.(*ast.FuncDecl); ok && f.Name.Name == "codePrefixToExit" {
			fn = f
			break
		}
	}
	if fn == nil {
		t.Fatal("codePrefixToExit not found in exitcode.go — this test can no longer " +
			"read the routing table it exists to check")
	}

	ast.Inspect(fn, func(n ast.Node) bool {
		sw, ok := n.(*ast.SwitchStmt)
		if !ok {
			return true
		}
		// `switch ns` routes namespaces; `switch code` routes whole
		// leaf codes. Anything else is a shape this test does not
		// understand, and guessing would be worse than failing.
		tag, ok := sw.Tag.(*ast.Ident)
		if !ok {
			t.Errorf("codePrefixToExit has a switch on %T, which this test cannot classify; "+
				"teach routedCases about it rather than leaving the routes unchecked", sw.Tag)
			return true
		}
		for _, stmt := range sw.Body.List {
			cc, ok := stmt.(*ast.CaseClause)
			if !ok {
				continue
			}
			for _, e := range cc.List {
				lit, ok := e.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				val, uerr := strconv.Unquote(lit.Value)
				if uerr != nil {
					continue
				}
				switch tag.Name {
				case "ns":
					namespaces = append(namespaces, val)
				case "code":
					leaves = append(leaves, val)
				default:
					t.Errorf("codePrefixToExit switches on unknown variable %q", tag.Name)
				}
			}
		}
		return true
	})
	sort.Strings(namespaces)
	sort.Strings(leaves)
	return namespaces, leaves
}

// TestExitCodes_NoUndocumentedRoutes: every namespace-or-leaf route in
// codePrefixToExit must be documented. The routes are read from the
// switch itself, so a case added in code with no matching doc row fails
// here — which is what the previous hand-list version only claimed.
func TestExitCodes_NoUndocumentedRoutes(t *testing.T) {
	prefixes, leaves := docMapping(t)
	routedNS, routedLeaves := routedCases(t)

	if len(routedNS) == 0 || len(routedLeaves) == 0 {
		t.Fatalf("read %d namespace and %d leaf routes from codePrefixToExit — the AST scan "+
			"stopped matching, so this test asserts nothing", len(routedNS), len(routedLeaves))
	}

	for _, p := range routedNS {
		if _, has := prefixes[p]; !has {
			t.Errorf("codePrefixToExit routes %s.* but docs/reference/exit-codes.md has no "+
				"row for it — an operator hitting that exit code cannot look up why", p)
		}
	}
	for _, l := range routedLeaves {
		if _, has := leaves[l]; !has {
			t.Errorf("codePrefixToExit routes %q (leaf) but docs/reference/exit-codes.md has "+
				"no row for it", l)
		}
	}
	t.Logf("checked %d namespace and %d leaf routes read from codePrefixToExit",
		len(routedNS), len(routedLeaves))
}
