package backup_test

import (
	"context"
	"errors"
	"io"
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

// Injection fires just BEFORE the tombstone becomes visible.
//
// It used to hook the `<key>.tmp.<rand>` staging Put, relying on the
// window between staging and the rename. The tombstone is now
// published by a single conditional PUT (issue #45), so no such window
// exists and hooking the temporary stopped injecting anything — the
// tests passed vacuously with the race never simulated.
//
// Hooking the tombstone key itself, before delegating, reproduces the
// same interleaving: the hold lands while the tombstone is being
// written but before it is durable, so PutHold's own tombstone guard
// still sees nothing and the post-tombstone re-check is what must
// catch it.

func (s *batchHoldInjectSP) Put(ctx context.Context, key string, r io.Reader, opts storage.PutOptions) (storage.PutResult, error) {
	if key == s.tombstoneKey {
		s.once.Do(s.inject)
	}
	return s.StoragePlugin.Put(ctx, key, r, opts)
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
