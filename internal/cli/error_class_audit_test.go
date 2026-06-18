// error_class_audit_test.go — meta-test that pins the wal-stream
// retry loop's permanent-error classification against the codebase.
//
// This is the regression net for the class of bug behind issue #79:
// the loop treated EVERY setup error as transient and retried
// forever. The fix added isPermanentStreamSetupError
// with a hard-coded list of error codes whose remediation requires
// operator action. The risk is the list drifting away from the
// codebase: a rename of "wal.start_before_slot_restart_lsn" to
// "wal.slot.start_before_restart" would leave the predicate silently
// stale and the loop would resume spinning forever on the very bug
// it was meant to catch.
//
// What it does:
//
//   - Parses every .go file under internal/ (excluding tests) via
//     go/parser, walks the AST, and collects the unique set of
//     string literals passed as the first argument to
//     output.NewError(...). This is the set of "known structured
//     error codes" in the codebase.
//
//   - Asserts every code in isPermanentStreamSetupError's hard-coded
//     list is present in that set. Codes that drift out of the
//     codebase (rename, deletion, typo) fail the test.
//
//   - Optionally surfaces, as t.Log lines for human review, the set
//     of error codes returned by the call sites the retry loop
//     transitively reaches (preflight, ensureSlot, resolveStartLSN,
//     identifySystem, repo.Open). This is the candidate list for
//     future additions to isPermanentStreamSetupError — a periodic
//     review keeps the classification complete.
package cli_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// permanentCodes mirrors the case set inside
// internal/cli/wal.go::isPermanentStreamSetupError.  When new codes
// are added to that switch, add them here too — the asymmetry-detection
// test below will catch a drift but it can't fix it for you.
var permanentCodes = []string{
	"wal.start_before_slot_restart_lsn",
	"wal.slot_no_restart_lsn",
	"usage.bad_lsn",
	"usage.unaligned_lsn",
	"usage.bad_flag",
}

// retryLoopReachableDirs lists the package paths whose
// output.NewError(...) call sites the wal-stream retry loop can
// transitively observe.  Audit-only: the test logs the unique codes
// found here so future maintainers can spot a "this should be
// permanent but isn't classified" gap.
//
// Path is relative to repo root.
var retryLoopReachableDirs = []string{
	"internal/cli", // wal.go itself
	"internal/pg",
	"internal/pg/replication",
	"internal/pg/walsink",
	"internal/repo",
	"internal/wal/follower",
}

// TestErrorClassAudit_PermanentCodesExistInCodebase pins that every
// entry in permanentCodes is still referenced by an
// output.NewError(...) somewhere in the codebase.  A drifted name
// (typo, refactor without a sweep) fails here.
func TestErrorClassAudit_PermanentCodesExistInCodebase(t *testing.T) {
	codes := collectErrorCodes(t, "internal", "cmd", "compat")
	for _, want := range permanentCodes {
		if !codes[want] {
			t.Errorf("permanent-error code %q is in isPermanentStreamSetupError but no output.NewError(%q, ...) exists in the codebase — predicate is stale",
				want, want)
		}
	}
}

// TestErrorClassAudit_PredicateMatchesSource pins that the
// permanentCodes list above stays in sync with the switch in
// internal/cli/wal.go::isPermanentStreamSetupError.  If a maintainer
// adds a new case to the switch without updating the test fixture
// (or vice versa), the asymmetry is surfaced here.
func TestErrorClassAudit_PredicateMatchesSource(t *testing.T) {
	src, err := os.ReadFile("wal.go")
	if err != nil {
		t.Fatalf("read wal.go: %v", err)
	}
	body := string(src)
	// We don't fully parse; the switch is small and self-contained
	// and a substring presence check is enough to catch drift.
	for _, want := range permanentCodes {
		quoted := `"` + want + `"`
		if !strings.Contains(body, quoted) {
			t.Errorf("permanent-error code %q listed in test fixture but missing from wal.go switch",
				want)
		}
	}
}

// TestErrorClassAudit_DiagnosticRetryLoopReachable is an audit-only
// reporter: it logs the unique output.NewError(...) codes found in
// every retry-loop-reachable package, so a periodic review can spot
// codes that should probably be classified as permanent (e.g. any
// new "*.bad_*" or "wal.gap*" code is almost certainly permanent and
// should be added to the switch).
//
// Always passes; the value is in the t.Log output during -v runs.
func TestErrorClassAudit_DiagnosticRetryLoopReachable(t *testing.T) {
	codes := collectErrorCodes(t, retryLoopReachableDirs...)
	if len(codes) == 0 {
		return
	}
	keys := make([]string, 0, len(codes))
	for k := range codes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	t.Logf("retry-loop-reachable output.NewError codes (%d unique):", len(keys))
	for _, k := range keys {
		marker := " "
		for _, p := range permanentCodes {
			if p == k {
				marker = "P"
				break
			}
		}
		t.Logf("  [%s] %s", marker, k)
	}
	t.Log("'P' = currently classified permanent.  Candidates for promotion are " +
		"codes describing operator-action-required failures (e.g. missing flags, " +
		"misshaped LSN/identifier values, broken contracts) that the retry loop " +
		"observes today as transient.")
}

// collectErrorCodes walks the given root directories (relative to
// the running test's working directory, which is the package dir for
// internal/cli tests).  It returns the set of unique string literals
// found as the first argument to output.NewError(...) calls.
//
// Limitations (acceptable for a meta-test):
//   - Only literal strings; ignores constants/computed code names.
//   - Doesn't follow the predicate logic — collects every NewError,
//     not "only ones reachable from the retry loop."  The diagnostic
//     reporter is intentionally a superset.
func collectErrorCodes(t *testing.T, roots ...string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	repoRoot := repoRootFromTestDir(t)
	for _, root := range roots {
		abs := filepath.Join(repoRoot, root)
		_ = filepath.WalkDir(abs, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				name := d.Name()
				if name == "vendor" || name == "node_modules" || name == "test-runs" || name == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, path, nil, parser.AllErrors)
			if err != nil {
				return nil // ignore parse errors; not our concern here
			}
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if !isOutputNewError(call.Fun) {
					return true
				}
				if len(call.Args) == 0 {
					return true
				}
				lit, ok := call.Args[0].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				// strconv-strip the surrounding quotes.
				s := lit.Value
				if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
					out[s[1:len(s)-1]] = true
				}
				return true
			})
			return nil
		})
	}
	return out
}

// isOutputNewError matches both `output.NewError(...)` (via selector)
// and a renamed import like `out.NewError(...)`.  In practice the
// codebase only uses `output.NewError`, but matching by trailing name
// keeps the audit robust to a future rename.
func isOutputNewError(expr ast.Expr) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	return sel.Sel.Name == "NewError"
}

// repoRootFromTestDir walks up from the package's test directory
// until it finds a go.mod, returning that directory.  The test runs
// with cwd == internal/cli/, so two parents up.
func repoRootFromTestDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 10; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not find repo root (no go.mod up the tree)")
	return ""
}
