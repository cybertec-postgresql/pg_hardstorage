package cli_test

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// `show <dep> latest` resolves to the newest live backup, as restore and
// verify do. The encryption tutorial piped it into jq; doctest checked
// only jq's exit status, so the "backup \"latest\" not found" failure
// went unnoticed until doctest learned pipefail.
func TestShow_LatestResolvesToNewestBackup(t *testing.T) {
	w := newReadWorld(t)
	commitVerifiableBackup(t, w, "db1", 0, []byte("older"))
	newest := commitVerifiableBackup(t, w, "db1", 1, []byte("newer"))
	out, errb, exit := runCLI(t, "show", "db1", "latest", "--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("show db1 latest: exit %d\n%s", exit, errb)
	}
	if !strings.Contains(out, newest) {
		t.Errorf("show latest did not show the newest backup %s:\n%s", newest, out)
	}
	if _, _, exit := runCLI(t, "show", "nodep", "latest", "--repo", w.repoURL, "-o", "json"); exit != int(output.ExitNotFound) {
		t.Errorf("show latest for a deployment with no backups: exit %d, want %d", exit, output.ExitNotFound)
	}
}
