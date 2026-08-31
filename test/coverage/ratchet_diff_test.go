package coverage_test

// The coverage ratchet is a release-gate check, and until now its own
// comparison had never been executed by anything — the same
// unwitnessed-code condition it exists to report.
//
// The behaviour under test, and why each part matters:
//
//   * line numbers are ignored. They used to be part of the key, which
//     made the gate cry wolf: editing anything above a function shifts
//     its line, so a commit that only ADDED a test presented every
//     untouched function below it as "newly unwitnessed". A gate that
//     fails for reasons unrelated to the change gets routed around.
//   * multiplicity is significant. One file legitimately holds several
//     same-named functions (five WriteText methods in
//     internal/cli/approval.go); witnessing one must read as a gain,
//     and regressing back must read as a violation. A set-based diff
//     would silently accept losing three of the four remaining.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const script = "../../scripts/coverage-ratchet-diff.sh"

func runDiff(t *testing.T, baseline, current []string) (out string, exit int) {
	t.Helper()
	dir := t.TempDir()
	write := func(name string, lines []string) string {
		p := filepath.Join(dir, name)
		body := strings.Join(lines, "\n")
		if body != "" {
			body += "\n"
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cmd := exec.Command("bash", script, write("base.txt", baseline), write("cur.txt", current))
	b, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok {
		exit = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running %s: %v\n%s", script, err, b)
	}
	return string(b), exit
}

func line(file string, ln int, fn string) string {
	return "github.com/x/y/" + file + ":" + itoa(ln) + ": " + fn
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for n > 0 {
		d = append([]byte{byte('0' + n%10)}, d...)
		n /= 10
	}
	return string(d)
}

// The regression this split was made for.
func TestRatchetDiff_LineDriftIsNotAViolation(t *testing.T) {
	base := []string{line("a.go", 10, "Foo"), line("a.go", 40, "Bar"), line("b.go", 7, "Baz")}
	// Same functions, every line shifted by a test added above them.
	cur := []string{line("a.go", 62, "Foo"), line("a.go", 92, "Bar"), line("b.go", 59, "Baz")}

	out, exit := runDiff(t, base, cur)
	if exit != 0 {
		t.Fatalf("line drift alone failed the gate (exit %d) — the false red that makes "+
			"a ratchet get routed around:\n%s", exit, out)
	}
	if !strings.Contains(out, "ratchet ok") {
		t.Errorf("expected a clean verdict:\n%s", out)
	}
}

func TestRatchetDiff_NewUnwitnessedFunctionViolates(t *testing.T) {
	base := []string{line("a.go", 10, "Foo")}
	cur := []string{line("a.go", 10, "Foo"), line("c.go", 3, "Fresh")}

	out, exit := runDiff(t, base, cur)
	if exit == 0 {
		t.Fatalf("a newly unwitnessed function did not fail the gate:\n%s", out)
	}
	if !strings.Contains(out, "c.go: Fresh") {
		t.Errorf("violation must name the offending function:\n%s", out)
	}
	if strings.Contains(out, "a.go") {
		t.Errorf("violation must not name functions that were already in the baseline:\n%s", out)
	}
}

// Set semantics would accept this; multiset must not.
func TestRatchetDiff_RegressingOneOfSeveralSameNamedFunctionsViolates(t *testing.T) {
	base := []string{line("a.go", 10, "WriteText"), line("a.go", 40, "WriteText")}
	cur := []string{
		line("a.go", 10, "WriteText"), line("a.go", 40, "WriteText"), line("a.go", 70, "WriteText"),
	}

	out, exit := runDiff(t, base, cur)
	if exit == 0 {
		t.Fatalf("a third unwitnessed WriteText in the same file was accepted — the diff is "+
			"set-based, so losing coverage on same-named functions goes unnoticed:\n%s", out)
	}
}

func TestRatchetDiff_WitnessingOneOfSeveralIsReportedAsAGain(t *testing.T) {
	base := []string{
		line("a.go", 10, "WriteText"), line("a.go", 40, "WriteText"), line("a.go", 70, "WriteText"),
	}
	cur := []string{line("a.go", 10, "WriteText"), line("a.go", 40, "WriteText")}

	out, exit := runDiff(t, base, cur)
	if exit != 0 {
		t.Fatalf("shrinking the list failed the gate (exit %d):\n%s", exit, out)
	}
	if !strings.Contains(out, "now witnessed") || !strings.Contains(out, "a.go: WriteText") {
		t.Errorf("a gain must be reported so the baseline gets updated:\n%s", out)
	}
}

func TestRatchetDiff_EmptyCurrentListIsAllGain(t *testing.T) {
	out, exit := runDiff(t, []string{line("a.go", 10, "Foo")}, nil)
	if exit != 0 {
		t.Fatalf("an empty dead-corner list must pass (exit %d):\n%s", exit, out)
	}
	if !strings.Contains(out, "now witnessed") {
		t.Errorf("expected the gain report:\n%s", out)
	}
}

// Blank lines are an editing artefact, not a function; they must not be
// compared (an empty key on one side would read as a violation).
func TestRatchetDiff_BlankLinesAreIgnored(t *testing.T) {
	base := []string{line("a.go", 10, "Foo"), "", "  "}
	cur := []string{line("a.go", 99, "Foo")}

	out, exit := runDiff(t, base, cur)
	if exit != 0 {
		t.Fatalf("blank lines in the baseline failed the gate (exit %d):\n%s", exit, out)
	}
	if strings.Contains(out, "now witnessed") {
		t.Errorf("blank lines must not be reported as witnessed functions:\n%s", out)
	}
}
