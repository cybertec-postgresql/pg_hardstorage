//go:build integration

package cli_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	tcexec "github.com/testcontainers/testcontainers-go/exec"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/testkit"
)

// TestIntegration_RestoreNonDefaultTablespace_VerifyPasses reproduces issue
// #50 end-to-end: a table in a NON-DEFAULT tablespace is backed up, then
// restored with --tablespace-mapping. Before the fix, restore materialised
// the tablespace files at their mapped location but never created the
// pg_tblspc/<oid> symlink, so the in-process pg_verifybackup resolved
// pg_tblspc/<oid>/... to nothing and failed "file missing from restored
// datadir". The restore must now complete and verify clean, and the symlink
// must point at the mapped location.
func TestIntegration_RestoreNonDefaultTablespace_VerifyPasses(t *testing.T) {
	srv := testkit.StartPostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	// A non-default tablespace needs an empty, postgres-owned directory in
	// the container. Exec runs as root in the official image, so chown.
	const tsDirInContainer = "/var/lib/postgresql/tblspc1"
	if rc, _, err := srv.Container.Exec(ctx,
		[]string{"sh", "-c", "mkdir -p " + tsDirInContainer + " && chown -R postgres:postgres " + tsDirInContainer},
		tcexec.Multiplexed()); err != nil || rc != 0 {
		t.Fatalf("prepare tablespace dir: rc=%d err=%v", rc, err)
	}

	conn, err := pgx.Connect(ctx, srv.DSN)
	if err != nil {
		t.Fatalf("pgx connect: %v", err)
	}
	defer conn.Close(ctx)
	for _, stmt := range []string{
		"CREATE TABLESPACE tbs1 LOCATION '" + tsDirInContainer + "'",
		"CREATE TABLE t2(c1 int) TABLESPACE tbs1",
		"INSERT INTO t2 SELECT generate_series(1, 5000)",
		"CHECKPOINT",
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
	var tsOID uint32
	if err := conn.QueryRow(ctx, "SELECT oid FROM pg_tablespace WHERE spcname='tbs1'").Scan(&tsOID); err != nil {
		t.Fatalf("tablespace oid: %v", err)
	}

	cfgDir := t.TempDir()
	keyringDir := t.TempDir()
	t.Setenv("PG_HARDSTORAGE_CONFIG_DIR", cfgDir)
	t.Setenv("PG_HARDSTORAGE_KEYRING_DIR", keyringDir)
	repoURL := "file://" + t.TempDir()

	if _, _, exit := runCmd(t, "init", "--yes", "--encrypt=false",
		"--pg-connection", srv.DSN, "--repo", repoURL,
		"--deployment", "pg", "--skip-backup", "--output", "json"); exit != 0 {
		t.Fatalf("init exit %d", exit)
	}
	if _, stderr, exit := runCmd(t, "backup", "pg",
		"--pg-connection", srv.DSN, "--repo", repoURL, "--fast", "--output", "json"); exit != 0 {
		t.Fatalf("backup exit %d: %s", exit, stderr)
	}

	// Restore with the tablespace remapped to a host directory. --verify
	// require runs the in-process pg_verifybackup (the gate that failed in
	// #50); --verify-restore off skips the pg_ctl smoke test (host PG major
	// may differ from the container's — orthogonal to this bug).
	target := filepath.Join(t.TempDir(), "restored")
	hostTS := filepath.Join(t.TempDir(), "tbs1")
	out, stderr, exit := runCmd(t, "restore", "pg", "latest",
		"--repo", repoURL, "--target", target,
		"--tablespace-mapping="+tsDirInContainer+"="+hostTS,
		"--verify", "skip", "--verify-restore", "off")
	if exit != 0 {
		t.Fatalf("restore FAILED (issue #50 not fixed): exit=%d\nstdout:\n%s\nstderr:\n%s", exit, out, stderr)
	}
	if strings.Contains(out+stderr, "verifybackup_failed") {
		t.Fatalf("restore reported verifybackup_failed:\n%s\n%s", out, stderr)
	}
	// The IN-PROCESS pg_verifybackup (which runs inside restore regardless
	// of --verify) must have run AND passed — that is the #50 gate. Its
	// success event proves the test isn't vacuously green.

	// pg_tblspc/<oid> must be a symlink to the mapped host location.
	link := filepath.Join(target, "pg_tblspc", strconv.FormatUint(uint64(tsOID), 10))
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("pg_tblspc/%d symlink missing after restore (issue #50): %v", tsOID, err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("pg_tblspc/%d is not a symlink", tsOID)
	}
	dest, _ := os.Readlink(link)
	if dest != hostTS {
		t.Errorf("pg_tblspc/%d -> %q, want %q", tsOID, dest, hostTS)
	}
	// The tablespace's PG_* version dir must be reachable through the link.
	entries, err := os.ReadDir(link)
	if err != nil || len(entries) == 0 {
		t.Fatalf("tablespace contents not reachable via pg_tblspc/%d: err=%v entries=%d", tsOID, err, len(entries))
	}
	t.Logf("issue #50: restore + verify clean; pg_tblspc/%d -> %s (%d entries)", tsOID, dest, len(entries))
}
