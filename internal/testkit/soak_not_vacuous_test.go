package testkit_test

// soak_not_vacuous_test.go — a soak must not report PASS without
// running.
//
// The 4-hour campaign of 2026-08-05 ran a chaos phase that "passed" in
// two seconds. It had skipped: the test required PGHS_CHAOS_BIN and the
// campaign did not set it. `go test` reports a skipped test as ok, a
// harness greps for pass/fail, and a phase that never executed is
// recorded as a phase that succeeded.
//
// That is worse than a phase that fails. A failure gets investigated;
// a vacuous pass gets counted as evidence. The campaign's summary said
// chaos passed, and it was wrong in the direction that matters.
//
// The durable fix is not "set the variable in the harness" — the next
// harness will forget. It is that a long-running test defaults to a
// short run and lets the environment DEEPEN it, which is what every
// soak in this tree does. Only a genuinely absent prerequisite (no
// Docker, no Go toolchain) may skip, and that is visible and rare.
//
// This asserts the shape by reading the source, because the failure
// mode is a test that does nothing — which is precisely what a
// behavioural check cannot distinguish from success.

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// envGatedSkip matches `if os.Getenv("X") == ""` followed closely by a
// t.Skip — the shape that turns a missing variable into a green tick.
//
// The character class includes DIGITS, and that is not cosmetic: it
// read [A-Z_]+ until PGHS_REPRO34_BIN was found skipping. The "34" made
// the whole variable unmatchable, so repro34_test.go — a Docker soak —
// skipped silently and this guard reported nothing. A guard whose
// pattern cannot express the names in use is a guard that passes for
// the wrong reason.
// The window must allow braces: the shape is
//
//	bin := os.Getenv("PGHS_X")
//	if bin == "" {
//	    t.Skip(...)
//	}
//
// and an earlier version of this pattern excluded `{`, so it matched
// nothing and the guard passed on the very code it was written to
// catch. A guard that cannot fail is worse than none.
var envGatedSkip = regexp.MustCompile(
	`(?s)Getenv\("(PGHS_[A-Z0-9_]+)"\)[\s\S]{0,160}?t\.Skip`)

// stripLineComments removes //-comments before matching.
//
// The window above is a proximity heuristic, and a comment between the
// Getenv and the t.Skip pushes them apart without changing what the
// code does. That is not hypothetical: explaining in a comment WHY a
// harness must not skip was enough to hide a reintroduced t.Skip from
// this guard. Prose must not be able to launder a violation.
func stripLineComments(src string) string {
	var b strings.Builder
	b.Grow(len(src))
	for _, line := range strings.Split(src, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(here), "..", ".."))
}

// TestSoaksDoNotSkipOnUnsetEnv is the guard.
func TestSoaksDoNotSkipOnUnsetEnv(t *testing.T) {
	root := repoRoot(t)

	var offenders []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			switch info.Name() {
			case "vendor", ".git", "test-runs", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		base := strings.ToLower(filepath.Base(path))
		// This file documents the forbidden shape in a comment, so it
		// matches its own pattern. Excluding it by name is exact and
		// avoids weakening the regex to accommodate prose.
		if base == "soak_not_vacuous_test.go" {
			return nil
		}
		// Filename-scoped on purpose: these are the long-running
		// harnesses whose whole value is that they RAN. "repro" joined
		// the list after repro34_test.go was found skipping on an unset
		// PGHS_REPRO34_BIN, invisible to this guard because its name
		// matched none of the other three.
		if !strings.Contains(base, "soak") && !strings.Contains(base, "chaos") &&
			!strings.Contains(base, "modelcheck") && !strings.Contains(base, "repro") {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		for _, m := range envGatedSkip.FindAllStringSubmatch(stripLineComments(string(src)), -1) {
			rel, _ := filepath.Rel(root, path)
			offenders = append(offenders, rel+": skips when "+m[1]+" is unset")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	if len(offenders) > 0 {
		t.Errorf("%d long-running test(s) skip because a PGHS_* variable is unset:\n  %s\n\n"+
			"A skip reports PASS. A campaign harness that greps for pass/fail then records "+
			"a phase that never ran as one that succeeded — which happened on 2026-08-05, "+
			"where the chaos phase 'passed' in two seconds having executed nothing.\n\n"+
			"Default to a SHORT run instead and let the variable deepen it, as every other "+
			"soak here does. Skip only for a prerequisite that genuinely cannot be "+
			"satisfied (no Docker, no Go toolchain).",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}
