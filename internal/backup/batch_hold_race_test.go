package backup_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// batchHoldInjectSP installs a hold on watchID the first time watchID's
// tombstone tmp body is written (before it is durable), simulating a
// `hold add` racing SoftDeleteBatch inside the pre-check→post-write window.
type batchHoldInjectSP struct {
	storage.StoragePlugin
	tombstoneKey string
	inject       func()
	once         sync.Once
}

func (s *batchHoldInjectSP) Put(ctx context.Context, key string, r io.Reader, opts storage.PutOptions) (storage.PutResult, error) {
	res, err := s.StoragePlugin.Put(ctx, key, r, opts)
	if err == nil && strings.HasPrefix(key, s.tombstoneKey+".tmp.") {
		s.once.Do(s.inject)
	}
	return res, err
}

// TestSoftDeleteBatch_HoldPlacedDuringBatchRollsBack pins bug 18 (batch
// side): a legal hold installed on a batch member after the pre-check but
// during the tombstone writes is caught by the post-write hold re-check,
// which rolls back the whole batch and refuses — a held backup is never
// silently tombstoned by a batch delete.
func TestSoftDeleteBatch_HoldPlacedDuringBatchRollsBack(t *testing.T) {
	store, sp, signer, _ := newStore(t)
	ctx := context.Background()

	// Two unrelated full backups (no chain between them).
	for _, id := range []string{"X", "Y"} {
		m := sampleManifest()
		m.Deployment = "db1"
		m.BackupID = id
		m.Type = backup.BackupTypeFull
		m.ParentBackupID = ""
		if err := store.Commit(ctx, m, signer, backup.CommitOptions{}); err != nil {
			t.Fatalf("commit %s: %v", id, err)
		}
	}

	wrapped := &batchHoldInjectSP{
		StoragePlugin: sp,
		// Watch X's tombstone tmp write; at that moment X is not yet
		// durable, so PutHold's own tombstone guard does not refuse — the
		// hold lands and the batch's post-write hold re-check must catch it.
		tombstoneKey: backup.TombstonePath("db1", "X"),
		inject: func() {
			_ = store.PutHold(ctx, "db1", "X", "ops", "litigation hold")
		},
	}
	batchStore := backup.NewManifestStore(wrapped)

	_, err := batchStore.SoftDeleteBatch(ctx, "db1", []string{"X", "Y"}, "manual", "routine")
	var held *backup.ManifestHeldError
	if !errors.As(err, &held) {
		t.Fatalf("batch should refuse with *ManifestHeldError when a hold is placed concurrently; got %T (%v)", err, err)
	}

	// Both members must have rolled back: live again.
	for _, id := range []string{"X", "Y"} {
		dead, derr := store.IsTombstoned(ctx, "db1", id)
		if derr != nil {
			t.Fatal(derr)
		}
		if dead {
			t.Errorf("%s must NOT be tombstoned after the batch rolled back", id)
		}
	}
}
