package cli_test

import (
	stdjson "encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// TestRepoInit_WORMFlags_RecordsPolicy: --worm-mode +
// --worm-retention land in HSREPO and surface in repo check.
func TestRepoInit_WORMFlags_RecordsPolicy(t *testing.T) {
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	url := "file://" + repoDir
	stdout, _, exit := runCLI(t,
		"repo", "init", url,
		"--worm-mode", "compliance",
		"--worm-retention", "7y",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("repo init: exit=%d\n%s", exit, stdout)
	}

	// repo check should surface the WORM block.
	stdout, _, exit = runCLI(t,
		"repo", "check", url,
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("repo check: exit=%d\n%s", exit, stdout)
	}
	var env output.Result
	if err := stdjson.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout)
	}
	body, _ := stdjson.Marshal(env.Result)
	bs := string(body)
	for _, want := range []string{
		`"worm":`,
		`"mode":"compliance"`,
		`"retention":"7y"`,
	} {
		if !strings.Contains(bs, want) {
			t.Errorf("repo check body missing %q:\n%s", want, bs)
		}
	}
}

// TestRepoInit_WORMFlags_RejectsHalfSet: --worm-mode without
// --worm-retention is a usage error (so an operator running
// "init --worm-mode compliance" doesn't end up with no policy
// silently).
func TestRepoInit_WORMFlags_RejectsHalfSet(t *testing.T) {
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "repo")
	os.MkdirAll(repoDir, 0o755)
	url := "file://" + repoDir
	for _, args := range [][]string{
		{"repo", "init", url, "--worm-mode", "compliance", "-o", "json"},
		{"repo", "init", url, "--worm-retention", "7y", "-o", "json"},
	} {
		_, stderr, exit := runCLI(t, args...)
		if exit != int(output.ExitMisuse) {
			t.Errorf("args=%v: exit=%d, want Misuse\nstderr=%s", args, exit, stderr)
		}
	}
}

// TestRepoInit_WORMFlags_RejectsBadMode: --worm-mode must be
// compliance or governance.
func TestRepoInit_WORMFlags_RejectsBadMode(t *testing.T) {
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "repo")
	os.MkdirAll(repoDir, 0o755)
	url := "file://" + repoDir
	_, stderr, exit := runCLI(t,
		"repo", "init", url,
		"--worm-mode", "loose",
		"--worm-retention", "7y",
		"-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("bad mode should exit Misuse; got %d\n%s", exit, stderr)
	}
}

// TestRepoInit_WORMFlags_RejectsBadRetention: --worm-retention
// must parse as a recognised duration.
func TestRepoInit_WORMFlags_RejectsBadRetention(t *testing.T) {
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "repo")
	os.MkdirAll(repoDir, 0o755)
	url := "file://" + repoDir
	_, stderr, exit := runCLI(t,
		"repo", "init", url,
		"--worm-mode", "compliance",
		"--worm-retention", "tomorrow",
		"-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("bad retention should exit Misuse; got %d\n%s", exit, stderr)
	}
}

// TestRepoCheck_WORMSurfaceInTextMode: text-mode repo check
// surfaces the WORM policy in a human-readable line.
func TestRepoCheck_WORMSurfaceInTextMode(t *testing.T) {
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "repo")
	os.MkdirAll(repoDir, 0o755)
	url := "file://" + repoDir
	if _, _, exit := runCLI(t,
		"repo", "init", url,
		"--worm-mode", "governance",
		"--worm-retention", "30d",
	); exit != int(output.ExitOK) {
		t.Fatalf("repo init failed")
	}
	stdout, _, exit := runCLI(t,
		"repo", "check", url,
		"-o", "text")
	if exit != int(output.ExitOK) {
		t.Fatalf("repo check: exit=%d\n%s", exit, stdout)
	}
	if !strings.Contains(stdout, "WORM policy:") {
		t.Errorf("text mode missing WORM line:\n%s", stdout)
	}
	if !strings.Contains(stdout, "governance") {
		t.Errorf("text mode missing governance mode:\n%s", stdout)
	}
}

// TestRepoCheck_NoWORMOmitsLine: a repo without WORM doesn't
// emit a WORM line in text mode (the absence is informative).
func TestRepoCheck_NoWORMOmitsLine(t *testing.T) {
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "repo")
	os.MkdirAll(repoDir, 0o755)
	url := "file://" + repoDir
	if _, _, exit := runCLI(t, "repo", "init", url); exit != int(output.ExitOK) {
		t.Fatal("init")
	}
	stdout, _, exit := runCLI(t, "repo", "check", url, "-o", "text")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit=%d\n%s", exit, stdout)
	}
	if strings.Contains(stdout, "WORM policy:") {
		t.Errorf("WORM line should be absent for repos without policy:\n%s", stdout)
	}
}
