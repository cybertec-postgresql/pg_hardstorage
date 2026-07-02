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

// injectOnTombstoneTmpSP fires inject() the first time a tombstone tmp body
// is written for the watched key (before the rename makes it visible) —
// simulating a concurrent action (child commit / hold add) that lands inside
// the cascade's pre-scan→post-tombstone-rescan window.
type injectOnTombstoneTmpSP struct {
	storage.StoragePlugin
	tombstoneKey string
	inject       func()
	once         sync.Once
}

func (s *injectOnTombstoneTmpSP) Put(ctx context.Context, key string, r io.Reader, opts storage.PutOptions) (storage.PutResult, error) {
	res, err := s.StoragePlugin.Put(ctx, key, r, opts)
	if err == nil && strings.HasPrefix(key, s.tombstoneKey+".tmp.") {
		s.once.Do(s.inject)
	}
	return res, err
}

// TestSoftDeleteCascade_ChildCommittedDuringCascadeRollsBack pins bug 17:
// SoftDeleteCascade's post-tombstone re-scan catches an incremental child
// that committed concurrently against a link in the pre-scan→tombstone
// window, rolls back the entire cascade, and refuses — so no orphaned chain
// (tombstoned-parent, live-child) survives.
func TestSoftDeleteCascade_ChildCommittedDuringCascadeRollsBack(t *testing.T) {
	store, sp, signer, _ := newStore(t)
	ctx := context.Background()

	// Chain A → B. Cascade will tombstone B then A.
	commitChain(t, store, signer, "db1", []chainLink{
		{id: "A", btype: backup.BackupTypeFull},
		{id: "B", parent: "A", btype: backup.BackupTypeIncremental},
	})

	wrapped := &injectOnTombstoneTmpSP{
		StoragePlugin: sp,
		// Watch leaf B's tombstone tmp (the FIRST cascade write). At this
		// instant neither B nor A is durably tombstoned yet, so the child
		// C (parent A) commits and stays live through Commit's own parent
		// re-check. The cascade then tombstones A, and the post-tombstone
		// re-scan must find C as a live descendant and roll everything back.
		tombstoneKey: backup.TombstonePath("db1", "B"),
		inject: func() {
			mc := sampleManifest()
			mc.Deployment = "db1"
			mc.BackupID = "C"
			mc.Type = backup.BackupTypeIncremental
			mc.ParentBackupID = "A"
			_ = store.Commit(ctx, mc, signer, backup.CommitOptions{})
		},
	}
	casStore := backup.NewManifestStore(wrapped)

	_, err := casStore.SoftDeleteCascade(ctx, "db1", "A", "manual", "routine")
	var live *backup.ChainHasLiveDescendantsError
	if !errors.As(err, &live) {
		t.Fatalf("cascade should refuse with *ChainHasLiveDescendantsError when a child commits concurrently; got %T (%v)", err, err)
	}

	// The whole cascade must have rolled back: A and B are live again.
	for _, id := range []string{"A", "B"} {
		dead, derr := store.IsTombstoned(ctx, "db1", id)
		if derr != nil {
			t.Fatal(derr)
		}
		if dead {
			t.Errorf("%s must NOT be tombstoned after the cascade rolled back", id)
		}
	}
}

// TestSoftDeleteCascade_HoldPlacedDuringCascadeRollsBack pins bug 18 (cascade
// side): a legal hold installed concurrently on a chain link, after the
// pre-flight hold check but during the tombstone writes, is caught by the
// post-tombstone hold re-check, which rolls the cascade back and refuses — so
// a held backup is never silently tombstoned by a cascade.
func TestSoftDeleteCascade_HoldPlacedDuringCascadeRollsBack(t *testing.T) {
	store, sp, signer, _ := newStore(t)
	ctx := context.Background()

	commitChain(t, store, signer, "db1", []chainLink{
		{id: "A", btype: backup.BackupTypeFull},
		{id: "B", parent: "A", btype: backup.BackupTypeIncremental},
	})

	wrapped := &injectOnTombstoneTmpSP{
		StoragePlugin: sp,
		// Watch leaf B's tombstone tmp write (the FIRST cascade write).
		// The inject fires before B's tombstone is durable, so PutHold's
		// own tombstone guard does not refuse — the hold lands, and the
		// cascade's post-tombstone hold re-check is what must catch it.
		tombstoneKey: backup.TombstonePath("db1", "B"),
		inject: func() {
			_ = store.PutHold(ctx, "db1", "B", "ops", "litigation hold")
		},
	}
	casStore := backup.NewManifestStore(wrapped)

	_, err := casStore.SoftDeleteCascade(ctx, "db1", "A", "manual", "routine")
	var held *backup.ChainHasHeldLinksError
	if !errors.As(err, &held) {
		t.Fatalf("cascade should refuse with *ChainHasHeldLinksError when a hold is placed concurrently; got %T (%v)", err, err)
	}

	// The whole cascade must have rolled back: A and B are live again.
	for _, id := range []string{"A", "B"} {
		dead, derr := store.IsTombstoned(ctx, "db1", id)
		if derr != nil {
			t.Fatal(derr)
		}
		if dead {
			t.Errorf("%s must NOT be tombstoned after the hold re-check rolled the cascade back", id)
		}
	}
}
