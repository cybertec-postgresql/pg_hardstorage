package main_test

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// findBinary locates the built pg_hardstorage_simple binary
// relative to the repo root.  Tests skip when the binary isn't
// built — running `go test ./...` shouldn't fail just because
// the operator hasn't run `make build-simple` yet, and CI does
// build it explicitly before running the test suite.
func findBinary(t *testing.T) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	// here is cmd/pg_hardstorage_simple/main_test.go; up three
	// = repo root.
	root := filepath.Clean(filepath.Join(filepath.Dir(here), "..", ".."))
	bin := filepath.Join(root, "bin", "pg_hardstorage_simple")
	if _, err := exec.LookPath(bin); err != nil {
		t.Skipf("binary not built at %s — run `make build-simple` (%v)", bin, err)
	}
	return bin
}

// TestBinary_HelpFlag: the binary takes --help and only --help (plus
// --version).  Smoke test that the parse-and-print path works and
// exits zero.
func TestBinary_HelpFlag(t *testing.T) {
	out, err := exec.Command(findBinary(t), "--help").CombinedOutput()
	if err != nil {
		t.Fatalf("--help: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "interactive backup helper") {
		t.Errorf("help text missing:\n%s", out)
	}
}

// TestBinary_VersionFlag: --version prints the version string.
// Mirrors the full pg_hardstorage binary's contract so packaging
// tools can rely on it.
func TestBinary_VersionFlag(t *testing.T) {
	out, err := exec.Command(findBinary(t), "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("--version: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) == "" {
		t.Errorf("--version printed nothing")
	}
}

// TestBinary_RejectsUnknownFlag: the binary's whole promise is that
// it doesn't have flags.  An attempted "--do backup" — the kind of
// scripting surface we explicitly refuse — should exit non-zero with
// a "unknown argument" diagnostic.
func TestBinary_RejectsUnknownFlag(t *testing.T) {
	cmd := exec.Command(findBinary(t), "--do", "backup")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected non-zero exit; got success.  stdout:\n%s", out)
	}
	if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() == 0 {
		t.Errorf("expected exit code 2, got %v", err)
	}
	if !strings.Contains(string(out), "unknown argument") {
		t.Errorf("expected 'unknown argument' diagnostic:\n%s", out)
	}
}
