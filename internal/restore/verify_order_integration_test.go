//go:build integration

package restore_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/runner"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
)

// Regression (H47): with both gates on — the CLI default — the boot
// test ran first, IN the target: it rewrote pg_control, consumed
// backup_label and left postverify-postgres.log behind, so the
// pg_verifybackup run afterwards failed every such restore (auto:
// status "failed"; require: exit 9). pg_verifybackup must see the
// directory exactly as restored, and the boot test must still pass.
func TestIntegration_PGVerifyBackupRunsBeforeBootTest(t *testing.T) {
	localBin(t, "pg_verifybackup")
	src := startLocalSource(t)
	src.SQL(t, "CREATE TABLE t AS SELECT g FROM generate_series(1,1000) g")
	repoURL, signer, verifier := testRepo(t)
	stubWALFetch(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	b, err := runner.Take(ctx, runner.TakeOptions{
		PGConnString: src.DSN, RepoURL: repoURL, Deployment: "db1",
		Signer: signer, Verifier: verifier, Fast: true, IncludeWAL: true,
	})
	if err != nil {
		t.Fatalf("Take: %v", err)
	}

	target := filepath.Join(t.TempDir(), "restored")
	res, err := restore.Restore(ctx, restore.Options{
		RepoURL: repoURL, Deployment: "db1", BackupID: b.BackupID,
		TargetDir: target, Verifier: verifier,
		PGVerifyBackup: restore.VerifyRequire,
		VerifyMode:     "required",
	})
	if err != nil {
		t.Fatalf("Restore with pg_verifybackup=require + boot test=required: %v", err)
	}
	if res.Verify == nil || res.Verify.Status != "passed" {
		t.Fatalf("pg_verifybackup result = %+v, want passed", res.Verify)
	}
	if _, err := os.Stat(filepath.Join(target, "postverify-postgres.log")); err == nil {
		t.Errorf("boot test left postverify-postgres.log in the restored data dir")
	}
}
