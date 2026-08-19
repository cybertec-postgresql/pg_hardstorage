//go:build mutation_runner

package mutation_test

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/mutation"
)

// TestMutationsAreCaught is the harness entry point.  Run with:
//
//	go test -tags=mutation_runner ./internal/testkit/mutation/...
//
// The test loops over mutation.Registry, invokes
//
//	go test -tags=<mutation.Tag> -count=1 -timeout=120s <packages>
//
// for each, and asserts the sub-process exits non-zero.  A mutation
// that doesn't trip any assertion in the named package is a
// coverage gap; this test fails hard for it.
//
// Why a build-tag guard on this file: we don't want this harness
// running under the project's normal CI test run because:
//
//   - It shells out to `go test`, so it pulls in the toolchain at
//     test time.  Faster tests don't.
//   - Each mutation costs the wallclock of one full sub-package
//     test run.  Three mutations = ~3-5 s.  Acceptable when an
//     operator opts in via the tag, expensive in the default
//     short-run CI.
//
// CI invokes `go test -tags=mutation_runner ./internal/testkit/mutation/...`
// as a separate stage (the SPEC's "L4 weekly" tier).
func TestMutationsAreCaught(t *testing.T) {
	if len(mutation.Registry) == 0 {
		t.Fatal("mutation.Registry is empty — no coverage")
	}
	for _, m := range mutation.Registry {
		t.Run(m.Tag, func(t *testing.T) {
			args := []string{"test",
				"-tags=" + m.Tag,
				"-count=1",
				"-timeout=120s",
			}
			// Focus narrows the subprocess to the catching test(s).
			// Without it a slow package (internal/cli: ~1200 tests,
			// some Docker-backed) burns its whole 120s budget even
			// after the catching test has failed, because Go keeps
			// running the remaining tests.
			if m.Focus != "" {
				args = append(args, "-run", m.Focus)
			}
			args = append(args, m.Packages...)
			cmd := exec.Command("go", args...)
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			err := cmd.Run()
			out := stdout.String() + stderr.String()
			if err == nil {
				if strings.Contains(out, "no tests to run") {
					t.Errorf("mutation %s: Focus %q matched no tests in %v — the "+
						"catching test was renamed or the regex is stale. "+
						"Update the Registry entry.\n%s",
						m.Tag, m.Focus, m.Packages, indent(out))
					return
				}
				t.Errorf("MUTATION COVERAGE GAP: %s\n  description: %s\n  packages: %v\n"+
					"  expected `go test -tags=%s` to FAIL but it passed.\n"+
					"  Either the mutation is a no-op or the test suite doesn't catch it.\n"+
					"  stdout:\n%s\n  stderr:\n%s",
					m.Tag, m.Description, m.Packages, m.Tag,
					indent(stdout.String()), indent(stderr.String()))
				return
			}
			// Non-zero exit is what we wanted.  Sanity-check that
			// the output looks like a real test failure (not a
			// build error) so a regression in the mutation file
			// itself doesn't masquerade as "caught".
			if strings.Contains(out, "build failed") ||
				strings.Contains(out, "[build failed]") ||
				strings.Contains(out, "syntax error") {
				t.Errorf("mutation %s caused a BUILD failure, not a test failure:\n%s",
					m.Tag, indent(out))
				return
			}
			if !strings.Contains(out, "FAIL") {
				t.Errorf("mutation %s exited non-zero but output doesn't look like a "+
					"test failure:\n%s", m.Tag, indent(out))
			}
		})
	}
}

func indent(s string) string {
	if s == "" {
		return "(empty)"
	}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "    " + l
	}
	return strings.Join(lines, "\n")
}
