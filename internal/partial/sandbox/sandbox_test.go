package sandbox_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/partial/sandbox"
)

// TestStart_RequiresDataDir is the obvious validation guard.
func TestStart_RequiresDataDir(t *testing.T) {
	if _, err := sandbox.Start(context.Background(), sandbox.Options{}); err == nil {
		t.Error("empty DataDir should error")
	}
}

// TestStart_RejectsMissingDataDir surfaces a clear error when the
// DataDir doesn't exist.
func TestStart_RejectsMissingDataDir(t *testing.T) {
	_, err := sandbox.Start(context.Background(), sandbox.Options{
		DataDir: "/definitely-does-not-exist-pg_hardstorage-test",
	})
	if err == nil {
		t.Error("missing DataDir should error")
	}
	if !strings.Contains(err.Error(), "stat DataDir") {
		t.Errorf("error should explain: %v", err)
	}
}

// TestStart_FailsWhenPGCtlMissing confirms the binary-discovery
// failure surfaces a clear message. We can't easily make pg_ctl
// "missing" from PATH in CI, but we can override PGCtlPath to a
// known-bad path.
func TestStart_FailsWhenPGCtlPathInvalid(t *testing.T) {
	dataDir := t.TempDir()
	_, err := sandbox.Start(context.Background(), sandbox.Options{
		DataDir:    dataDir,
		PGCtlPath:  "/definitely-not-pg_ctl",
		PGDumpPath: "/definitely-not-pg_dump",
	})
	if err == nil {
		t.Error("invalid pg_ctl path should error")
	}
}

// TestDiscoverPGTools_HonoursPATH: when both binaries are present,
// returns their paths. When not, returns an error.
//
// This test is environment-dependent: skips when pg_ctl/pg_dump
// aren't installed (the common CI case).
func TestDiscoverPGTools(t *testing.T) {
	pgCtl, pgDump, err := sandbox.DiscoverPGTools()
	if err != nil {
		t.Skipf("PG tools not on PATH (skipping): %v", err)
	}
	if pgCtl == "" || pgDump == "" {
		t.Errorf("paths should be non-empty: pgCtl=%q pgDump=%q", pgCtl, pgDump)
	}
	// Sanity: both should be absolute paths after LookPath.
	if !filepath.IsAbs(pgCtl) || !filepath.IsAbs(pgDump) {
		t.Errorf("LookPath should return absolute paths: pgCtl=%s pgDump=%s",
			pgCtl, pgDump)
	}
}

// TestStart_RestoresAutoConfBackup: when an existing auto.conf is
// present in the data dir, Start backs it up and the cleanup
// (which runs even on an error path) restores it. Exercises the
// backup-and-restore-auto.conf logic without needing a working PG.
func TestStart_RestoresAutoConfBackup(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.Chmod(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	originalConf := []byte("# operator's existing auto.conf\nfoo = 'bar'\n")
	autoConfPath := filepath.Join(dataDir, "postgresql.auto.conf")
	if err := os.WriteFile(autoConfPath, originalConf, 0o600); err != nil {
		t.Fatal(err)
	}

	// Start will fail because pg_ctl can't actually start a PG
	// against an empty dir, but the cleanup path on the failure
	// must restore our backup.
	_, err := sandbox.Start(context.Background(), sandbox.Options{
		DataDir:    dataDir,
		PGCtlPath:  "/bin/false", // any non-pg_ctl binary that exits non-zero
		PGDumpPath: "/bin/false",
	})
	if err == nil {
		t.Fatal("expected start to fail against empty data dir")
	}

	// auto.conf should be restored to the original contents.
	got, rerr := os.ReadFile(autoConfPath)
	if rerr != nil {
		t.Fatalf("auto.conf should still exist post-cleanup: %v", rerr)
	}
	if string(got) != string(originalConf) {
		t.Errorf("auto.conf was not restored\n  got: %q\n want: %q", got, originalConf)
	}

	// Backup file should NOT be left behind.
	if _, err := os.Stat(autoConfPath + ".pg_hardstorage_sandbox_backup"); err == nil {
		t.Error("backup file should have been removed during cleanup")
	}
}

// TestStart_NoExistingAutoConf_RemovesOurOwn: when there's no
// existing auto.conf and Start fails, the auto.conf we wrote is
// removed (no leftover litter).
func TestStart_NoExistingAutoConf_RemovesOurOwn(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.Chmod(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	autoConfPath := filepath.Join(dataDir, "postgresql.auto.conf")

	_, err := sandbox.Start(context.Background(), sandbox.Options{
		DataDir:    dataDir,
		PGCtlPath:  "/bin/false",
		PGDumpPath: "/bin/false",
	})
	if err == nil {
		t.Fatal("expected error against empty data dir")
	}
	if _, err := os.Stat(autoConfPath); err == nil {
		t.Errorf("auto.conf should be removed (we wrote it)")
	}
}

// TestStart_SocketDirCleanedOnFailure: the temp socket dir is
// removed when Start errors out (no leaked tempdirs).
func TestStart_SocketDirCleanedOnFailure(t *testing.T) {
	dataDir := t.TempDir()
	os.Chmod(dataDir, 0o700)

	// Capture the tempdir count before + after.
	before, _ := filepath.Glob(filepath.Join(os.TempDir(), "pg_hardstorage-sandbox-sock-*"))

	_, err := sandbox.Start(context.Background(), sandbox.Options{
		DataDir:    dataDir,
		PGCtlPath:  "/bin/false",
		PGDumpPath: "/bin/false",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	after, _ := filepath.Glob(filepath.Join(os.TempDir(), "pg_hardstorage-sandbox-sock-*"))
	if len(after) > len(before) {
		t.Errorf("Start leaked a socket tempdir: before=%d after=%d", len(before), len(after))
	}
}
