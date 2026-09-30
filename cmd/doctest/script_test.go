package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runBlocks executes buildScript's output for bodies and returns each
// block's recorded exit status.
func runBlocks(t *testing.T, bodies ...string) []string {
	t.Helper()
	dir := t.TempDir()
	var blocks []block
	for i, b := range bodies {
		blocks = append(blocks, block{file: "x.md", line: i + 1, body: b})
	}
	script, _ := buildScript(blocks, dir, fileEnv{})
	p := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", p)
	cmd.Env = append(os.Environ(), "DOCTEST_TMPDIR="+dir, "PG_HARDSTORAGE_BIN=/bin/true")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("script: %v\n%s", err, out)
	}
	var got []string
	for i := range bodies {
		b, err := os.ReadFile(filepath.Join(dir, "block."+pad3(i)+".exit"))
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, strings.TrimSpace(string(b)))
	}
	return got
}

func pad3(i int) string {
	s := "00" + string(rune('0'+i))
	return s[len(s)-3:]
}

// A block's status was only its LAST command's ($? under set +e, no
// pipefail): a tutorial block whose first command failed and whose last
// was an echo passed, as did a failing command piped into anything.
func TestBlockStatusSeesEveryCommand(t *testing.T) {
	got := runBlocks(t,
		"false\necho after", // an earlier command failed
		"false | cat",       // a failing pipeline stage
		"echo ok\ntrue",     // all good
		"exit_code() { return 3; }\nexit_code\necho after", // failing function
		"if false; then :; fi\nfalse || true",              // tested failures are not failures
	)
	want := []string{"1", "1", "0", "3", "0"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("block %d exit = %s, want %s", i, got[i], want[i])
		}
	}
}

// State still carries across blocks, as tutorials rely on.
func TestBlocksShareShellState(t *testing.T) {
	got := runBlocks(t, "export DT_X=1\nmkdir -p sub && cd sub", `[ "$DT_X" = 1 ] && [ "$(basename "$PWD")" = sub ]`)
	if got[1] != "0" {
		t.Errorf("the second block lost the first block's env/cwd (exit %s)", got[1])
	}
}
