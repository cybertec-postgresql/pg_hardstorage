package chat

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// The cheatsheet tells the model which commands take no `--repo`. A
// list of facts about the CLI rots the moment the CLI changes, and a
// rotted entry here is worse than no entry: the model is being told
// something false by the thing that exists to stop it inventing.
//
// Why the list exists at all: `--repo` sits on roughly half the
// command surface, and the command catalog in the same prompt shows
// verbs without flags — so the model has nothing to reason from and
// guesses. Across a 194-question evaluation it was the most-invented
// flag by a wide margin, 42-69 occurrences per model, every one
// producing a command line that dies with `unknown flag: --repo`.

var repoTrapEntry = regexp.MustCompile("(?m)^- `([a-z][a-z0-9 -]*)` — ")

// TestRepoTrapListMatchesTheBinary asserts every command the
// cheatsheet claims takes no --repo genuinely takes none, by asking
// the built binary.
func TestRepoTrapListMatchesTheBinary(t *testing.T) {
	bin := findBinary(t)
	if bin == "" {
		t.Skip("pg_hardstorage binary not built; run `make build` to exercise this test")
	}

	sheet := FlagCheatsheet()
	idx := strings.Index(sheet, "### The `--repo` trap")
	if idx < 0 {
		t.Fatal("the --repo trap section is missing from the cheatsheet — " +
			"it is the counter-signal for the most-invented flag in the product")
	}
	// Bound the scan to this section: later sections list commands
	// that DO take --repo.
	section := sheet[idx:]
	if end := strings.Index(section, "\nIf you are about to write"); end > 0 {
		section = section[:end]
	}

	claims := repoTrapEntry.FindAllStringSubmatch(section, -1)
	if len(claims) == 0 {
		t.Fatal("no commands listed in the --repo trap section")
	}

	for _, c := range claims {
		cmd := strings.Fields(c[1])
		out, _ := exec.Command(bin, append(cmd, "--help")...).CombinedOutput()
		if strings.Contains(string(out), "--repo ") || strings.Contains(string(out), "--repo\t") {
			t.Errorf("cheatsheet claims `%s` takes no --repo, but the binary's help lists one. "+
				"A false entry here actively misleads the model.", c[1])
		}
	}
	t.Logf("verified %d commands against the binary", len(claims))
}

// findBinary locates the built CLI without hardcoding a layout.
func findBinary(t *testing.T) string {
	t.Helper()
	for _, p := range []string{
		"../../../bin/pg_hardstorage",
		"../../../../bin/pg_hardstorage",
	} {
		if out, err := exec.Command(p, "version").CombinedOutput(); err == nil && len(out) > 0 {
			return p
		}
	}
	return ""
}
