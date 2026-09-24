package backup

import (
	"context"
	"errors"
	"testing"
)

// Undelete checked the ancestors only BEFORE removing the child's
// tombstone. A SoftDelete of the parent that ran its own checks while
// the child was still tombstoned (so it saw no live descendant) and
// landed before the flip left a live incremental on a tombstoned
// parent — listed as restorable, refused by every restore, and
// permanently lost once the parent's chunks age past GC grace. The
// ancestors must be re-checked AFTER the flip and the flip rolled back.
func TestUndelete_ParentTombstonedDuringFlip_RollsBack(t *testing.T) {
	store, sp, signer, _ := raceWorld(t)
	ctx := context.Background()
	parent := commitRaceManifest(t, store, sp, signer, "db1.full.P", []byte("parent-chunk"))

	child := *parent
	child.BackupID = "db1.incr.C"
	child.Type = BackupTypeIncremental
	child.ParentBackupID = parent.BackupID
	child.Attestation = nil
	if err := store.Commit(ctx, &child, signer, CommitOptions{}); err != nil {
		t.Fatalf("commit child: %v", err)
	}
	if err := store.SoftDelete(ctx, "db1", child.BackupID, "manual", "oops"); err != nil {
		t.Fatal(err)
	}

	// The parent's SoftDelete lands in the window: its descendant
	// checks ran while the child was still tombstoned.
	undeleteTestHookAfterUnmark = func() {
		if _, err := store.softDeleteUnchecked(ctx, "db1", parent.BackupID, "manual", "concurrent"); err != nil {
			t.Errorf("stage parent tombstone: %v", err)
		}
	}
	defer func() { undeleteTestHookAfterUnmark = nil }()

	restored, err := store.Undelete(ctx, "db1", child.BackupID)
	var pErr *UndeleteParentTombstonedError
	if !errors.As(err, &pErr) || restored {
		t.Fatalf("Undelete = (%v, %v), want (false, UndeleteParentTombstonedError)", restored, err)
	}
	dead, err := store.IsTombstoned(ctx, "db1", child.BackupID)
	if err != nil || !dead {
		t.Fatalf("child left live (tombstoned=%v, err=%v) on a tombstoned parent", dead, err)
	}
}
