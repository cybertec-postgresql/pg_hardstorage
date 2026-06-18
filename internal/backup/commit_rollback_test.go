package backup_test

import (
	"context"
	"crypto/rand"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
)

// deleteFailingSP passes every operation through to the wrapped plugin
// except Delete, which always fails — to drive the commit-rollback
// failure path.
type deleteFailingSP struct {
	storage.StoragePlugin
}

func (d deleteFailingSP) Delete(_ context.Context, _ string) error {
	return errors.New("synthetic delete failure")
}

// TestCommit_RollbackFailureIsSurfaced pins poor-error-handling audit #2:
// when an incremental's post-write parent check fails AND the rollback
// Delete of the just-written manifest also fails, Commit must surface the
// rollback failure (a stray orphaned manifest remains) instead of
// silently dropping it — while still preserving the original cause for
// errors.As.
func TestCommit_RollbackFailureIsSurfaced(t *testing.T) {
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: t.TempDir()}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })

	priv, _, err := backup.GenerateKeypair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := backup.LoadSigner(priv)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	store := backup.NewManifestStore(sp)

	// Full parent A, then soft-delete it so it's no longer live.
	a := sampleManifest()
	a.BackupID = "db1.full.A"
	a.Type = backup.BackupTypeFull
	a.ParentBackupID = ""
	if err := store.Commit(ctx, a, signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("commit A: %v", err)
	}
	if err := store.SoftDelete(ctx, a.Deployment, a.BackupID, "manual", "test"); err != nil {
		t.Fatalf("soft-delete A: %v", err)
	}

	// Commit incremental B (parent A, now not live) through a store whose
	// rollback Delete fails. Parent check fails → rollback → Delete fails.
	failStore := backup.NewManifestStore(deleteFailingSP{sp})
	b := sampleManifest()
	b.BackupID = "db1.incr.B"
	b.Type = backup.BackupTypeIncremental
	b.ParentBackupID = a.BackupID

	err = failStore.Commit(ctx, b, signer, backup.CommitOptions{})
	if err == nil {
		t.Fatal("commit B should fail (orphaned incremental); got nil")
	}
	// Original cause still reachable for typed handling.
	var orphan *backup.OrphanedIncrementalError
	if !errors.As(err, &orphan) {
		t.Errorf("error should still unwrap to *OrphanedIncrementalError; got %T (%v)", err, err)
	}
	// AND the rollback failure is now visible, not dropped.
	if !strings.Contains(err.Error(), "stray") && !strings.Contains(err.Error(), "orphaned manifest remains") {
		t.Errorf("rollback-Delete failure must be surfaced (stray manifest); got: %v", err)
	}
}

// TestCommit_RollbackSucceeds_ReturnsCauseCleanly: when the rollback
// Delete succeeds, Commit returns exactly the original cause (a bare
// *OrphanedIncrementalError) with no rollback noise.
func TestCommit_RollbackSucceeds_ReturnsCauseCleanly(t *testing.T) {
	store, _, signer, _ := newStore(t)
	ctx := context.Background()

	a := sampleManifest()
	a.BackupID = "db1.full.A"
	a.Type = backup.BackupTypeFull
	a.ParentBackupID = ""
	if err := store.Commit(ctx, a, signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("commit A: %v", err)
	}
	if err := store.SoftDelete(ctx, a.Deployment, a.BackupID, "manual", "test"); err != nil {
		t.Fatalf("soft-delete A: %v", err)
	}

	b := sampleManifest()
	b.BackupID = "db1.incr.B"
	b.Type = backup.BackupTypeIncremental
	b.ParentBackupID = a.BackupID

	err := store.Commit(ctx, b, signer, backup.CommitOptions{})
	var orphan *backup.OrphanedIncrementalError
	if !errors.As(err, &orphan) {
		t.Fatalf("expected *OrphanedIncrementalError; got %T (%v)", err, err)
	}
	if strings.Contains(err.Error(), "stray") {
		t.Errorf("clean rollback must not mention a stray manifest; got %v", err)
	}
}
