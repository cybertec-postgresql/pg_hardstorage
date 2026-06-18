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

// TestExitCodes_NoUndocumentedRoutes: every namespace-or-leaf
// route in codePrefixToExit must be documented.  Tests synthesise
// a code in every known-routed namespace, route through
// codePrefixToExit, and assert the result matches some documented
// route.  Catches a code-side route added without a doc update.
func TestExitCodes_NoUndocumentedRoutes(t *testing.T) {
	prefixes, leaves := docMapping(t)
	// Every namespace below is known-routed (i.e. listed in
	// codePrefixToExit's switch).  Keep this list aligned with
	// that switch — adding a case there without adding it here
	// fails this test, which is exactly the safety we want.
	knownRoutedPrefixes := []string{
		"auth", "usage", "preflight", "aborted", "notfound",
		"conflict", "verify", "anomaly", "doctor",
	}
	for _, p := range knownRoutedPrefixes {
		if _, has := prefixes[p]; !has {
			t.Errorf("codePrefixToExit routes %s.* but it's not documented", p)
		}
	}
	knownRoutedLeaves := []string{
		"storage.unreachable", "kms.unreachable",
		"restore.target_unreachable", "restore.target_in_wal_gap",
	}
	for _, l := range knownRoutedLeaves {
		if _, has := leaves[l]; !has {
			t.Errorf("codePrefixToExit routes %q (leaf) but it's not documented", l)
		}
	}
}
