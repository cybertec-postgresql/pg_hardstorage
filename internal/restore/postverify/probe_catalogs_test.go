package postverify_test

// probe_catalogs_test.go — the L3 catalog probe passed over destroyed
// catalogs whenever psql printed anything.
//
// probeSelect1, twenty lines above probeCatalogs in the same file,
// documents the hazard exactly:
//
//	"CombinedOutput interleaves psql's stderr NOTICEs/WARNINGs (e.g.
//	 the collation-version mismatch warning when a cluster built
//	 against one glibc is started on another — routine when restoring
//	 a container-created backup onto a host) with the query result."
//
// and takes the LAST non-empty token accordingly. probeCatalogs did:
//
//	if v := strings.TrimSpace(string(out)); v == "0" || v == "" {
//		return fmt.Errorf("pg_database returned %q (catalogs corrupt?)", v)
//	}
//
// With any diagnostic present the blob is neither "0" nor "", so the
// check passed — including when the count really was 0, which is the
// single case this probe exists to catch. Its own comment calls that
// "catastrophic and means catalogs are toast".
//
// The failing combination is the ordinary one: restore a
// container-created backup onto a host (different glibc → collation
// warning on every connection) and the catalog check stops working.

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore/postverify"
)

// fakePsqlEmitting writes a stub psql that prints body verbatim and
// exits 0 — letting a test reproduce psql's real interleaving of
// diagnostics and result rows.
func fakePsqlEmitting(t *testing.T, dir, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the probe path is Unix-only; this harness uses /bin/sh")
	}
	path := filepath.Join(dir, "psql")
	script := "#!/bin/sh\ncat <<'EOF_BODY'\n" + body + "\nEOF_BODY\nexit 0\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// The bug: zero connectable databases, reported alongside the
// collation warning that its sibling documents as routine.
func TestProbeCatalogs_ZeroCountBehindAWarningIsCaught(t *testing.T) {
	psql := fakePsqlEmitting(t, t.TempDir(),
		`WARNING:  database "postgres" has a collation version mismatch
DETAIL:  The database was created using collation version 2.36, but the operating system provides version 2.39.
HINT:  Rebuild all objects in this database that use the default collation.
0`)

	err := postverify.ProbeCatalogsForTest(context.Background(), psql, "postgres:///postgres")
	if err == nil {
		t.Fatal("pg_database returned 0 connectable databases and the probe PASSED.\n\n" +
			"The count was hidden behind a collation-version warning — the exact diagnostic " +
			"probeSelect1 documents as routine when restoring a container-created backup " +
			"onto a host. The one catastrophic case this probe exists to catch is the one " +
			"it stopped catching.")
	}
	if !strings.Contains(err.Error(), "catalogs corrupt") {
		t.Errorf("error does not identify the catalog failure: %v", err)
	}
}

// The healthy case must still pass with the same diagnostics present,
// or the fix would break every container-to-host restore.
func TestProbeCatalogs_HealthyCountBehindAWarningPasses(t *testing.T) {
	psql := fakePsqlEmitting(t, t.TempDir(),
		`WARNING:  database "postgres" has a collation version mismatch
HINT:  Rebuild all objects in this database that use the default collation.
3`)

	if err := postverify.ProbeCatalogsForTest(context.Background(), psql, "postgres:///postgres"); err != nil {
		t.Fatalf("a healthy cluster failed the catalog probe because psql printed a warning: %v\n\n"+
			"That warning is routine on a container-to-host restore; refusing here would "+
			"break the common case.", err)
	}
}

// Clean output, no diagnostics — the case that always worked.
func TestProbeCatalogs_CleanOutput(t *testing.T) {
	dir := t.TempDir()
	if err := postverify.ProbeCatalogsForTest(context.Background(),
		fakePsqlEmitting(t, dir, "3"), "postgres:///postgres"); err != nil {
		t.Errorf("clean count rejected: %v", err)
	}
	if err := postverify.ProbeCatalogsForTest(context.Background(),
		fakePsqlEmitting(t, t.TempDir(), "0"), "postgres:///postgres"); err == nil {
		t.Error("a clean zero count was accepted")
	}
}

// A non-numeric answer is a failure, not a pass. Accepting anything
// non-"0" is how the original let the zero through in the first place.
func TestProbeCatalogs_NonNumericAnswerIsRefused(t *testing.T) {
	for name, body := range map[string]string{
		"only diagnostics": `WARNING:  something happened
HINT:  and nothing else`,
		"empty":     ``,
		"truncated": `WARNING: x` + "\n" + `psql: server closed the connection`,
	} {
		t.Run(name, func(t *testing.T) {
			err := postverify.ProbeCatalogsForTest(context.Background(),
				fakePsqlEmitting(t, t.TempDir(), body), "postgres:///postgres")
			if err == nil {
				t.Errorf("psql produced no usable count and the probe passed; the L3 gate "+
					"would report a verified cluster having read nothing (output was %q)", body)
			}
		})
	}
}
