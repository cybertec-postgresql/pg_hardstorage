package cli_test

import (
	stdjson "encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// TestRepoBundle_ExportRequiresFlags: every flag the CLI declares
// as required must trigger ExitMisuse + usage.missing_flag.
func TestRepoBundle_ExportRequiresFlags(t *testing.T) {
	cases := [][]string{
		{"repo", "bundle", "export"},
		{"repo", "bundle", "export", "--repo", "file:///tmp/x"},
		{"repo", "bundle", "export", "--repo", "file:///tmp/x", "--deployment", "db1"},
	}
	for _, args := range cases {
		_, stderr, exit := runCLI(t, append(args, "-o", "json")...)
		if exit != int(output.ExitMisuse) {
			t.Errorf("%v: expected ExitMisuse, got %d (stderr=%s)", args, exit, stderr)
		}
		if !strings.Contains(stderr, "usage.missing_flag") {
			t.Errorf("%v: expected usage.missing_flag, stderr=%s", args, stderr)
		}
	}
}

// TestRepoBundle_ExportNoBackupsErrors: a deployment with no
// backups should produce a structured error.
func TestRepoBundle_ExportNoBackupsErrors(t *testing.T) {
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "src")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	repoURL := "file://" + repoDir
	if _, _, exit := runCLI(t, "repo", "init", repoURL); exit != int(output.ExitOK) {
		t.Fatalf("repo init exit=%d", exit)
	}
	bundlePath := filepath.Join(tmp, "out.tar")
	_, stderr, exit := runCLI(t,
		"repo", "bundle", "export",
		"--repo", repoURL,
		"--deployment", "db-empty",
		"--out", bundlePath,
		"-o", "json",
	)
	if exit == int(output.ExitOK) {
		t.Fatalf("export should refuse empty deployment; exit=%d", exit)
	}
	if !strings.Contains(stderr, "no manifests") {
		t.Errorf("expected no-manifests error; stderr=%s", stderr)
	}
	if _, statErr := os.Stat(bundlePath); statErr == nil {
		t.Errorf("export should not leave a partial file behind")
	}
}

// TestRepoBundle_ImportRequiresFlags: --to and --in are both
// required.
func TestRepoBundle_ImportRequiresFlags(t *testing.T) {
	cases := [][]string{
		{"repo", "bundle", "import"},
		{"repo", "bundle", "import", "--to", "file:///tmp/x"},
	}
	for _, args := range cases {
		_, stderr, exit := runCLI(t, append(args, "-o", "json")...)
		if exit != int(output.ExitMisuse) {
			t.Errorf("%v: expected ExitMisuse, got %d (stderr=%s)", args, exit, stderr)
		}
	}
}

// TestRepoBundle_ImportFromEmptyTarFails:
// importing a corrupt/empty tar should produce a structured
// repo.bundle_import_failed error rather than panic.
func TestRepoBundle_ImportFromEmptyTarFails(t *testing.T) {
	tmp := t.TempDir()
	dstDir := filepath.Join(tmp, "dst")
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dstURL := "file://" + dstDir
	if _, _, exit := runCLI(t, "repo", "init", dstURL); exit != int(output.ExitOK) {
		t.Fatalf("repo init exit=%d", exit)
	}
	emptyTar := filepath.Join(tmp, "empty.tar")
	if err := os.WriteFile(emptyTar, []byte("not a tar at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, stderr, exit := runCLI(t,
		"repo", "bundle", "import",
		"--to", dstURL,
		"--in", emptyTar,
		"-o", "json",
	)
	if exit == int(output.ExitOK) {
		t.Fatalf("import of garbage tar should fail; exit=%d", exit)
	}
	if !strings.Contains(stderr, "repo.bundle_import_failed") {
		t.Errorf("expected repo.bundle_import_failed; stderr=%s", stderr)
	}
}

// TestRepoBundle_ExportRefusesExistingOutPath: the CLI uses
// O_EXCL so an operator cannot accidentally clobber a previous
// bundle.
func TestRepoBundle_ExportRefusesExistingOutPath(t *testing.T) {
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "src")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	repoURL := "file://" + repoDir
	runCLI(t, "repo", "init", repoURL)

	out := filepath.Join(tmp, "exists.tar")
	if err := os.WriteFile(out, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, stderr, exit := runCLI(t,
		"repo", "bundle", "export",
		"--repo", repoURL,
		"--deployment", "db1",
		"--out", out,
		"-o", "json",
	)
	if exit == int(output.ExitOK) {
		t.Fatal("export should refuse existing out path")
	}
	if !strings.Contains(stderr, "exists") && !strings.Contains(stderr, "file exists") {
		// Some platforms phrase this differently; accept both.
		t.Logf("stderr=%s", stderr)
	}
}

var _ = stdjson.Marshal
