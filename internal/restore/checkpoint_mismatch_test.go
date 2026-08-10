package restore_test

// checkpoint_mismatch_test.go — the guard that stops two backups'
// files from merging into one datadir.
//
// An interrupted restore leaves a checkpoint marker so the next
// attempt can resume. Resume MUST verify the checkpoint belongs to the
// backup being restored: resuming backup B's request into backup A's
// partial datadir would interleave two backups' files — a datadir that
// boots and serves silently-wrong data, the worst failure in the
// system. restore.Restore refuses with conflict.checkpoint_mismatch;
// that refusal had no direct test.

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"crypto/rand"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
)

func TestRestore_ResumeWithDifferentBackupRefused(t *testing.T) {
	root := t.TempDir()
	repoURL := "file://" + root
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatal(err)
	}
	sp := &fs.Plugin{}
	u, _ := url.Parse(repoURL)
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: u}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	priv, pub, _ := backup.GenerateKeypair(rand.Reader)
	signer, _ := backup.LoadSigner(priv)
	verifier, _ := backup.LoadVerifier(pub)

	// A real, restorable backup B (empty files is enough — the
	// checkpoint-mismatch guard fires before materialisation).
	ts := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	backupB := "db1.full.BEE." + ts.Format("20060102T150405Z")
	m := &backup.Manifest{
		Schema: backup.Schema, BackupID: backupB, Deployment: "db1", Tenant: "default",
		Type: backup.BackupTypeFull, PGVersion: 17, SystemIdentifier: "7000000000000000001",
		StartLSN: "0/3000028", StopLSN: "0/30001A0", Timeline: 1,
		Tablespaces: []backup.Tablespace{{OID: 1663, Location: "pg_default"}},
		StartedAt:   ts, StoppedAt: ts.Add(30 * time.Second),
		BackupLabel: "START WAL LOCATION: 0/3000028\n", Files: []backup.FileEntry{},
	}
	if err := backup.NewManifestStore(sp).Commit(context.Background(), m, signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("commit B: %v", err)
	}

	// A target holding ONLY a checkpoint from a DIFFERENT backup A —
	// the resume-eligible state the preflight admits.
	target := filepath.Join(t.TempDir(), "restored")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	cw := restore.NewCheckpointWriter(target, restore.Checkpoint{
		BackupID: "db1.full.AAA.20260101T000000Z", Deployment: "db1",
	}, 0)
	// Record one completed file so the writer is dirty — Flush is a no-op
	// on a clean writer, which is exactly what silently left the target
	// checkpoint-less on the first cut of this test.
	if err := cw.MarkFileDone("base/1/1259", 8192, 1); err != nil {
		t.Fatalf("mark file done: %v", err)
	}
	if err := cw.Flush(); err != nil {
		t.Fatalf("plant checkpoint: %v", err)
	}

	// Isolate setup from behaviour: prove the checkpoint is planted and
	// loadable before exercising Restore.
	if _, statErr := os.Stat(filepath.Join(target, restore.CheckpointFilename)); statErr != nil {
		t.Fatalf("checkpoint file not on disk after Flush: %v", statErr)
	}
	if pre, lerr := restore.LoadCheckpoint(target); lerr != nil || pre == nil || pre.BackupID != "db1.full.AAA.20260101T000000Z" {
		t.Fatalf("planted checkpoint not loadable: cp=%+v err=%v", pre, lerr)
	}

	_, err := restore.Restore(context.Background(), restore.Options{
		RepoURL: repoURL, Deployment: "db1", BackupID: backupB,
		TargetDir: target, Verifier: verifier,
	})
	if err == nil {
		t.Fatal("Restore resumed backup B into a target holding backup A's checkpoint WITHOUT " +
			"error — two backups' files would interleave into one datadir that boots and " +
			"serves silently-wrong data")
	}
	if !strings.Contains(err.Error(), "checkpoint_mismatch") {
		t.Errorf("wrong error (want conflict.checkpoint_mismatch): %v", err)
	}
	if !strings.Contains(err.Error(), "db1.full.AAA.20260101T000000Z") {
		t.Errorf("refusal should name the checkpoint's backup id for the resume remedy: %v", err)
	}
}
