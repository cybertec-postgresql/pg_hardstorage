package backup_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// A GC snapshot can select an old orphan immediately before a backup reuses
// it. The mutation fence must prevent publication during sweep, and the final
// existence check must prevent publication after GC removed the chunk.
func TestCommit_GCFencePreventsStaleSnapshotCorruption(t *testing.T) {
	store, sp, signer, _ := newStore(t)
	ctx := context.Background()
	body := []byte("old orphan reused by a new backup")
	h := repo.HashOf(body)
	if _, err := sp.Put(ctx, repo.ChunkKey(h), bytes.NewReader(body), storage.PutOptions{IfNotExists: true}); err != nil {
		t.Fatal(err)
	}

	m := sampleManifest()
	m.Files = []backup.FileEntry{{
		Path: "base/1/1", Size: int64(len(body)),
		Chunks: []backup.ChunkRef{{Hash: h, Offset: 0, Len: int64(len(body))}},
	}}

	gcLock, err := repo.AcquireMutationLock(ctx, sp, "test GC sweep")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(ctx, m, signer, backup.CommitOptions{RequireChunksPresent: true}); !errors.Is(err, repo.ErrMutationLocked) {
		t.Fatalf("commit during GC = %v, want ErrMutationLocked", err)
	}
	if err := sp.Delete(ctx, repo.ChunkKey(h)); err != nil {
		t.Fatal(err)
	}
	if err := gcLock.Release(ctx); err != nil {
		t.Fatal(err)
	}

	if err := store.Commit(ctx, m, signer, backup.CommitOptions{RequireChunksPresent: true}); err == nil {
		t.Fatal("commit after GC deleted a referenced chunk must fail")
	}
	if _, err := sp.Stat(ctx, backup.PrimaryPath(m.Deployment, m.BackupID)); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("primary manifest should not exist after fenced failure; stat=%v", err)
	}
}

func TestUndelete_GCFencePreventsReferenceResurrectionDuringSweep(t *testing.T) {
	store, sp, signer, _ := newStore(t)
	ctx := context.Background()
	m := sampleManifest()
	if err := store.Commit(ctx, m, signer, backup.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.SoftDelete(ctx, m.Deployment, m.BackupID, "test", "gc race"); err != nil {
		t.Fatal(err)
	}

	gcLock, err := repo.AcquireMutationLock(ctx, sp, "test GC sweep")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gcLock.Release(context.Background()) }()

	if _, err := store.Undelete(ctx, m.Deployment, m.BackupID); !errors.Is(err, repo.ErrMutationLocked) {
		t.Fatalf("undelete during GC = %v, want ErrMutationLocked", err)
	}
	if _, err := store.UndeleteForce(ctx, m.Deployment, m.BackupID); !errors.Is(err, repo.ErrMutationLocked) {
		t.Fatalf("force undelete during GC = %v, want ErrMutationLocked", err)
	}
	if dead, err := store.IsTombstoned(ctx, m.Deployment, m.BackupID); err != nil || !dead {
		t.Fatalf("tombstone changed while GC held fence: dead=%v err=%v", dead, err)
	}
}
