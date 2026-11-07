package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// readCommandsOnRepo are commands that need both the repository and the
// signing keyring; each takes the repo URL appended as --repo.
var readCommandsOnRepo = [][]string{
	{"repo", "check"},
	{"repo", "audit"},
	{"list", "db1"},
	{"status"},
	{"show", "db1", "latest"},
	{"verify", "db1", "latest"},
	{"anomaly", "check", "db1"},
	{"rotate", "db1"},
}

// A damaged HSREPO is reported as what it is — verify.repo_marker_corrupt,
// exit 9, with a repair hint — by every command, not as code "internal"
// with a raw JSON parse error.
func TestDamagedRepoMarker_IsNotAnInternalError(t *testing.T) {
	w := newReadWorld(t)
	marker := filepath.Join(strings.TrimPrefix(w.repoURL, "file://"), "HSREPO")
	if err := os.WriteFile(marker, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range readCommandsOnRepo {
		args := append(append([]string{}, c...), "--repo", w.repoURL, "-o", "json")
		_, stderr, exit := runCLI(t, args...)
		if exit != int(output.ExitVerifyFailed) || !strings.Contains(stderr, "verify.repo_marker_corrupt") {
			t.Errorf("%v: exit=%d, want %d with verify.repo_marker_corrupt\n%s", c, exit, output.ExitVerifyFailed, stderr)
		}
		if strings.Contains(stderr, `"internal"`) {
			t.Errorf("%v: still reported as an internal error\n%s", c, stderr)
		}
	}
}

// With an empty keyring, the repository is still diagnosed first: a URL
// with no repository is notfound.repo, a damaged one is
// verify.repo_marker_corrupt, and only a healthy repository gets as far
// as notfound.signing_key. All three used to be notfound.signing_key,
// so a script could not tell a wrong URL from a missing key.
func TestEmptyKeyring_RepositoryIsDiagnosedFirst(t *testing.T) {
	w := newReadWorld(t)
	damaged := t.TempDir()
	if err := os.WriteFile(filepath.Join(damaged, "HSREPO"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	missing := "file://" + filepath.Join(t.TempDir(), "nothing-here")

	// Point the keyring at an empty directory for the rest of the test.
	t.Setenv("PG_HARDSTORAGE_KEYRING_DIR", t.TempDir())

	for _, c := range readCommandsOnRepo {
		for _, tc := range []struct{ url, code string }{
			{missing, "notfound.repo"},
			{"file://" + damaged, "verify.repo_marker_corrupt"},
			{w.repoURL, "notfound.signing_key"},
		} {
			args := append(append([]string{}, c...), "--repo", tc.url, "-o", "json")
			_, stderr, _ := runCLI(t, args...)
			if !strings.Contains(stderr, tc.code) {
				t.Errorf("%v on %s: want %s\n%s", c, tc.url, tc.code, stderr)
			}
		}
	}
}
