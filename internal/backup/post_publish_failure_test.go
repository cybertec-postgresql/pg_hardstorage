package backup_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

type renameLandedErrorSP struct{ storage.StoragePlugin }

func (s *renameLandedErrorSP) RenameIfNotExists(ctx context.Context, src, dst string) error {
	if err := s.StoragePlugin.RenameIfNotExists(ctx, src, dst); err != nil {
		return err
	}
	if strings.HasSuffix(dst, "/manifest.json") {
		return errors.New("source cleanup failed after destination landed")
	}
	return nil
}

func TestCommit_AmbiguousRenameStillRunsParentSafety(t *testing.T) {
	_, raw, signer, _ := newStore(t)
	sp := &renameLandedErrorSP{StoragePlugin: raw}
	store := backup.NewManifestStore(sp)
	m := sampleManifest()
	m.Type = backup.BackupTypeIncremental
	m.ParentBackupID = "missing-parent"

	err := store.Commit(context.Background(), m, signer, backup.CommitOptions{})
	if err == nil {
		t.Fatal("orphaned incremental must be rejected even when rename reports a post-publish error")
	}
	if _, statErr := raw.Stat(context.Background(), backup.PrimaryPath(m.Deployment, m.BackupID)); !errors.Is(statErr, storage.ErrNotFound) {
		t.Fatalf("orphaned primary remained visible: %v", statErr)
	}
}

func TestCommit_ExistingIdenticalManifestStillReturnsAlreadyCommitted(t *testing.T) {
	store, _, signer, _ := newStore(t)
	if err := store.Commit(context.Background(), sampleManifest(), signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(context.Background(), sampleManifest(), signer, backup.CommitOptions{}); !errors.Is(err, backup.ErrAlreadyCommitted) {
		t.Fatalf("second commit = %v, want ErrAlreadyCommitted", err)
	}
}

type retentionFailSP struct{ storage.StoragePlugin }

func (s *retentionFailSP) SetRetention(context.Context, string, time.Time, storage.WORMMode) error {
	return errors.New("retention service unavailable")
}

func TestCommit_RetentionFailureRollsBackPrimary(t *testing.T) {
	_, raw, signer, _ := newStore(t)
	store := backup.NewManifestStore(&retentionFailSP{StoragePlugin: raw})
	m := sampleManifest()
	err := store.Commit(context.Background(), m, signer, backup.CommitOptions{
		RetainUntil: time.Now().Add(time.Hour), RetentionMode: storage.WORMCompliance,
	})
	if err == nil {
		t.Fatal("retention failure must fail commit")
	}
	if _, statErr := raw.Stat(context.Background(), backup.PrimaryPath(m.Deployment, m.BackupID)); !errors.Is(statErr, storage.ErrNotFound) {
		t.Fatalf("unlocked primary remained visible: %v", statErr)
	}
}
