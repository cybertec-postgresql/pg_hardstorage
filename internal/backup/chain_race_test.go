package backup_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// incrementalChildOf builds an incremental manifest of parent in
// deployment, using the sample manifest's shape (and chunks).
func incrementalChildOf(deployment, id, parent string) *backup.Manifest {
	m := sampleManifest()
	m.BackupID = id
	m.Deployment = deployment
	m.Type = backup.BackupTypeIncremental
	m.ParentBackupID = parent
	return m
}

// TestCommit_RefusesOrphanedIncremental pins the commit half of the
// chain-race fix (data-loss path #4): committing an incremental whose
// parent has been soft-deleted must FAIL with ErrOrphanedIncremental
// and ROLL THE COMMIT BACK, so the repo never holds a tombstoned-
// parent + live-child pair that restore can't reassemble.
func TestCommit_RefusesOrphanedIncremental(t *testing.T) {
	store, sp, signer, _ := newStore(t)
	commitChain(t, store, signer, "db1", []chainLink{
		{id: "P", btype: backup.BackupTypeFull},
	})
	if err := store.SoftDelete(context.Background(), "db1", "P", "manual", "test"); err != nil {
		t.Fatalf("SoftDelete parent: %v", err)
	}

	err := store.Commit(context.Background(), incrementalChildOf("db1", "C", "P"), signer, backup.CommitOptions{})
	if !errors.Is(err, backup.ErrOrphanedIncremental) {
		t.Fatalf("Commit of incremental on a tombstoned parent should fail with ErrOrphanedIncremental; got %v", err)
	}
	var oe *backup.OrphanedIncrementalError
	if !errors.As(err, &oe) || oe.ParentBackupID != "P" {
		t.Errorf("error should be *OrphanedIncrementalError naming parent P; got %#v", err)
	}
	// The just-written child manifest must have been rolled back.
	if _, serr := sp.Stat(context.Background(), backup.PrimaryPath("db1", "C")); !errors.Is(serr, storage.ErrNotFound) {
		t.Errorf("orphaned child manifest should have been rolled back; Stat err = %v (want ErrNotFound)", serr)
	}
}

// TestCommit_AllowsIncrementalWithLiveParent: the guard must not fire
// on the normal case — an incremental whose parent is live commits and
// is readable.
func TestCommit_AllowsIncrementalWithLiveParent(t *testing.T) {
	store, _, signer, verifier := newStore(t)
	commitChain(t, store, signer, "db1", []chainLink{
		{id: "P", btype: backup.BackupTypeFull},
	})
	if err := store.Commit(context.Background(), incrementalChildOf("db1", "C", "P"), signer, backup.CommitOptions{}); err != nil {
		t.Fatalf("incremental with a live parent should commit: %v", err)
	}
	if _, err := store.Read(context.Background(), "db1", "C", verifier); err != nil {
		t.Errorf("incremental C should be readable after commit: %v", err)
	}
}

// TestChainRace_NoOrphanUnderConcurrentDeleteAndBackup stresses the
// soft-delete-vs-incremental-backup race from both sides: for each
// iteration a parent is committed, then SoftDelete(parent) races
// Commit(child-of-parent). The write-then-verify guards on both sides
// (Commit re-checks the parent; SoftDelete re-scans after tombstoning)
// must guarantee the repo NEVER ends in the orphaned state — a
// tombstoned parent with a live child. Valid outcomes: the delete
// wins (child rolled back), the backup wins (parent stays live), or
// both refuse; never an orphan.
func TestChainRace_NoOrphanUnderConcurrentDeleteAndBackup(t *testing.T) {
	store, sp, signer, _ := newStore(t)
	ctx := context.Background()

	const iters = 250
	orphans := 0
	for i := 0; i < iters; i++ {
		dep := fmt.Sprintf("db-%d", i)
		parent := "P"
		child := "C"
		if err := store.Commit(ctx, fullManifest(dep, parent), signer, backup.CommitOptions{}); err != nil {
			t.Fatalf("iter %d: commit parent: %v", i, err)
		}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = store.SoftDelete(ctx, dep, parent, "manual", "race")
		}()
		go func() {
			defer wg.Done()
			_ = store.Commit(ctx, incrementalChildOf(dep, child, parent), signer, backup.CommitOptions{})
		}()
		wg.Wait()

		// Invariant: not (parent tombstoned AND child present-and-live).
		parentDead, err := store.IsTombstoned(ctx, dep, parent)
		if err != nil {
			t.Fatalf("iter %d: IsTombstoned parent: %v", i, err)
		}
		childPresent := false
		if _, serr := sp.Stat(ctx, backup.PrimaryPath(dep, child)); serr == nil {
			childPresent = true
		} else if !errors.Is(serr, storage.ErrNotFound) {
			t.Fatalf("iter %d: Stat child: %v", i, serr)
		}
		childDead := false
		if childPresent {
			if childDead, err = store.IsTombstoned(ctx, dep, child); err != nil {
				t.Fatalf("iter %d: IsTombstoned child: %v", i, err)
			}
		}
		if parentDead && childPresent && !childDead {
			orphans++
			t.Errorf("iter %d: ORPHAN — parent %s tombstoned but child %s is live", i, parent, child)
		}
	}
	if orphans > 0 {
		t.Fatalf("%d/%d iterations left an orphaned incremental chain", orphans, iters)
	}
}

// fullManifest builds a full-backup manifest (sample shape) for the
// given deployment + id.
func fullManifest(deployment, id string) *backup.Manifest {
	m := sampleManifest()
	m.BackupID = id
	m.Deployment = deployment
	m.Type = backup.BackupTypeFull
	m.ParentBackupID = ""
	return m
}
