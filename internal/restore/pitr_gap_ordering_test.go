//go:build integration

package restore_test

// pitr_gap_ordering_test.go — a PITR target inside an unarchived
// window must be refused, deterministically.
//
// Whether the WAL covering a backup's stop LSN reaches the repo depends
// on ORDER: `wal stream` creates its slot on first run, and until that
// slot exists nothing reserves WAL. Take the backup first and the
// segments written before the slot appears are recycled unarchived, so
// the backup's own stop LSN sits in a hole that PITR can never cross.
//
// TestIntegration_PITR_RestoreToLSN depended on winning that race by
// luck — the slot usually appeared while the cluster was still writing
// the backup's segment. Under parallel load it lost, and the honest
// refusal read as a test failure. These two pin BOTH halves of the
// contract deterministically, by forcing the WAL past the backup's
// segment before the streamer ever runs:
//
//   - no reservation  -> the hole is real and the restore must refuse
//   - reserved first  -> the same ordering leaves no hole
//
// The first half is the one that matters for data integrity: silently
// restoring to a target the repo cannot reach would hand back a
// cluster that stops short of where the operator asked, which is the
// class of failure the gap detector exists to prevent.

import (
	"context"
	"crypto/rand"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/runner"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/cli"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/pg/testkit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
)

func pitrWithOrdering(t *testing.T, reserveSlotFirst bool) error {
	srv := testkit.StartPostgres(t)
	repoURL := "file://" + t.TempDir()
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatal(err)
	}
	priv, pub, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	exec := func(sql string) error {
		c, cerr := pg.Connect(ctx, srv.DSN, pg.ModeRegular)
		if cerr != nil {
			return cerr
		}
		defer c.Close(ctx)
		return c.PgConn().ExecParams(ctx, sql, nil, nil, nil, nil).Read().Err
	}

	if reserveSlotFirst {
		if e := exec("SELECT pg_create_physical_replication_slot('pg_hardstorage_db1', true)"); e != nil {
			t.Fatalf("reserve slot: %v", e)
		}
	}

	bres, err := runner.Take(ctx, runner.TakeOptions{
		PGConnString: srv.DSN, RepoURL: repoURL, Deployment: "db1",
		Signer: signer, Verifier: verifier, Fast: true, IncludeManifest: true,
	})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}

	// Deterministically advance past the backup's own segment BEFORE
	// the streamer runs. This is what the flake does by luck.
	for i := 0; i < 3; i++ {
		if e := exec("SELECT pg_switch_wal()"); e != nil {
			t.Fatalf("switch wal: %v", e)
		}
	}
	if e := exec("CHECKPOINT"); e != nil {
		t.Fatalf("checkpoint: %v", e)
	}

	streamCtx, streamCancel := context.WithTimeout(ctx, 60*time.Second)
	defer streamCancel()
	go func() {
		for streamCtx.Err() == nil {
			_ = exec("INSERT INTO pg_class SELECT * FROM pg_class LIMIT 0")
			_ = exec("SELECT pg_switch_wal()")
			time.Sleep(200 * time.Millisecond)
		}
	}()
	root := cli.NewRoot()
	root.SetContext(streamCtx)
	root.SetArgs([]string{"wal", "stream", "db1",
		"--pg-connection", srv.DSN, "--repo", repoURL,
		"--status-interval", "250ms", "--once", "--output", "json"})
	if exit := cli.Run(root); exit != 0 {
		t.Fatalf("wal stream --once exit=%d", exit)
	}
	streamCancel()

	_, rerr := restore.Restore(ctx, restore.Options{
		RepoURL: repoURL, Deployment: "db1", BackupID: bres.BackupID,
		TargetDir: filepath.Join(t.TempDir(), "restored"), Verifier: verifier,
		Recovery: &restore.Recovery{
			Enable: true, TargetLSN: bres.StopLSN, Inclusive: true,
			Action: "pause", Timeline: "latest",
			RestoreCommand: "/usr/bin/pg_hardstorage wal fetch db1 %f %p --repo " + repoURL,
		},
	})
	return rerr
}

// Without the reservation the hole is real and the restore must refuse.
func TestPITR_TargetInUnarchivedWindow_Refused(t *testing.T) {
	err := pitrWithOrdering(t, false)
	if err == nil {
		t.Fatal("expected target_in_wal_gap; got a successful restore")
	}
	if !strings.Contains(err.Error(), "target_in_wal_gap") {
		t.Fatalf("expected target_in_wal_gap, got: %v", err)
	}
	t.Logf("reproduced deterministically: %v", err)
}

// With the reservation the same ordering leaves no hole.
func TestPITR_SlotReservedBeforeBackup_TargetHonoured(t *testing.T) {
	if err := pitrWithOrdering(t, true); err != nil {
		t.Fatalf("restore failed even though the slot was reserved first: %v", err)
	}
	t.Log("slot reserved before the backup: no gap, PITR target honoured")
}
