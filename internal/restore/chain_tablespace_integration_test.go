//go:build integration

package restore_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/runner"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
)

// Regression (C10): a full -> incremental chain restore of a cluster
// with a non-default tablespace. materializeManifestInto wrote every
// file — tablespace files included — into the link's staging root,
// ignoring FileEntry.TablespaceOID, and never created pg_tblspc/<oid>
// links, so pg_combinebackup never saw the tablespace: its data was
// misplaced (and colliding paths overwritten) while the merged
// manifest still verified and restore reported success.
//
// Real PostgreSQL end to end (PG 17+, local binaries): take a full and
// an incremental backup of a cluster whose table lives in a tablespace,
// chain-restore with the tablespace remapped, then check the layout,
// pg_verifybackup, and that the restored cluster boots and returns the
// rows written between the two backups.
func TestIntegration_ChainRestore_Tablespace(t *testing.T) {
	combineBin := localBin(t, "pg_combinebackup")
	localBin(t, "pg_verifybackup")
	if out, err := exec.Command(combineBin, "--version").CombinedOutput(); err != nil ||
		!strings.Contains(string(out), " 1") {
		t.Skipf("pg_combinebackup unusable: %v %s", err, out)
	}

	tsSrc := filepath.Join(t.TempDir(), "ts_src")
	if err := os.MkdirAll(tsSrc, 0o700); err != nil {
		t.Fatal(err)
	}
	src := startLocalSource(t, "summarize_wal = on")
	src.SQL(t, "CREATE TABLESPACE ts1 LOCATION '"+tsSrc+"'")
	oid := src.SQL(t, "SELECT oid FROM pg_tablespace WHERE spcname = 'ts1'")
	src.SQL(t, "CREATE TABLE t_ts (id int, v text) TABLESPACE ts1")
	src.SQL(t, "INSERT INTO t_ts SELECT g, 'full-' || g FROM generate_series(1,1000) g")
	src.SQL(t, "CREATE TABLE t_def AS SELECT g AS id FROM generate_series(1,100) g")
	src.SQL(t, "CHECKPOINT")

	repoURL, signer, verifier := testRepo(t)
	stubWALFetch(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	full, err := runner.Take(ctx, runner.TakeOptions{
		PGConnString: src.DSN, RepoURL: repoURL, Deployment: "db1",
		Signer: signer, Verifier: verifier, Fast: true, IncludeWAL: true,
	})
	if err != nil {
		t.Fatalf("full backup: %v", err)
	}
	_, sp, err := repo.Open(ctx, repoURL)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	fullM, err := backup.NewManifestStore(sp).Read(ctx, "db1", full.BackupID, verifier)
	if err != nil {
		t.Fatal(err)
	}

	src.SQL(t, "INSERT INTO t_ts SELECT g, 'inc-' || g FROM generate_series(1001,1500) g")
	src.SQL(t, "UPDATE t_ts SET v = 'changed' WHERE id <= 10")
	src.SQL(t, "CHECKPOINT")
	// Let the WAL summarizer cover the change before the incremental.
	src.SQL(t, "SELECT pg_switch_wal()")
	src.SQL(t, "CHECKPOINT")

	inc, err := runner.Take(ctx, runner.TakeOptions{
		PGConnString: src.DSN, RepoURL: repoURL, Deployment: "db1",
		Signer: signer, Verifier: verifier, Fast: true, IncludeWAL: true,
		Incremental: &runner.IncrementalConfig{ParentBackupID: full.BackupID, ParentPGManifest: fullM.PGBackupManifest},
	})
	if err != nil {
		t.Fatalf("incremental backup: %v", err)
	}

	tsDest := filepath.Join(t.TempDir(), "ts_restored")
	remap, err := restore.ParseTablespaceRemap([]string{tsSrc + "=" + tsDest})
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "restored")
	res, err := restore.Restore(ctx, restore.Options{
		RepoURL: repoURL, Deployment: "db1", BackupID: inc.BackupID, TargetDir: target,
		Verifier: verifier, TablespaceRemap: remap,
		ChainStagingRoot: filepath.Join(t.TempDir(), "staging"),
		PGVerifyBackup:   restore.VerifyRequire,
	})
	if err != nil {
		t.Fatalf("chain restore: %v", err)
	}
	if res.Verify == nil || res.Verify.Status != "passed" {
		t.Fatalf("pg_verifybackup on the merged datadir: %+v", res.Verify)
	}

	// Layout: pg_tblspc/<oid> -> remapped dir, which holds the data.
	link := filepath.Join(target, "pg_tblspc", oid)
	if dest, err := os.Readlink(link); err != nil || filepath.Clean(dest) != tsDest {
		t.Fatalf("pg_tblspc/%s -> %q (%v), want %s", oid, dest, err, tsDest)
	}
	entries, _ := os.ReadDir(tsDest)
	if len(entries) == 0 {
		t.Fatalf("remapped tablespace dir %s is empty", tsDest)
	}
	// No tablespace file may have been flattened into PGDATA.
	for _, e := range entries {
		if _, err := os.Stat(filepath.Join(target, e.Name())); err == nil {
			t.Errorf("tablespace dir %q also appears at the PGDATA root (misplaced)", e.Name())
		}
	}

	// Boot it and read the tablespace table back.
	pgCtl := localBin(t, "pg_ctl")
	sock, err := os.MkdirTemp("/tmp", "pghs-chk-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sock)
	logf := filepath.Join(t.TempDir(), "restored.log")
	if out, err := exec.Command(pgCtl, "-D", target, "-l", logf, "-w", "-t", "120",
		"-o", "-p 5432 -c listen_addresses='' -c unix_socket_directories="+sock, "start").CombinedOutput(); err != nil {
		log, _ := os.ReadFile(logf)
		t.Fatalf("start restored cluster: %v\n%s\n%s", err, out, log)
	}
	defer exec.Command(pgCtl, "-D", target, "-m", "immediate", "-w", "stop").Run()
	restored := &localSource{Sock: sock, psqlBin: src.psqlBin}
	if got := restored.SQL(t, "SELECT count(*) FROM t_ts"); got != "1500" {
		t.Errorf("t_ts rows = %s, want 1500 (incremental's inserts included)", got)
	}
	if got := restored.SQL(t, "SELECT count(*) FROM t_ts WHERE v = 'changed'"); got != "10" {
		t.Errorf("updated rows = %s, want 10", got)
	}
	if got := restored.SQL(t, "SELECT count(*) FROM t_def"); got != "100" {
		t.Errorf("t_def rows = %s, want 100", got)
	}
}
