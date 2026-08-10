package chain_test

// tombstoned_ancestor_test.go — the "live incremental on an
// unavailable ancestor" hazard is UNREACHABLE, and this pins why.
//
// The worry: `backup graph --include-deleted` is the view an operator
// reaches for while a restore is already failing. If a live
// incremental could sit on a TOMBSTONED parent, that view would link
// the two, emit no orphan issue (the parent is present, just marked
// deleted), and go quiet — showing a connected chain while `restore`
// refuses the leaf. A green diagnostic over an unrestorable reality:
// this campaign's signature failure shape.
//
// It cannot happen. Three guards, at the layer BELOW the graph,
// compose to make the state unconstructible — so the graph is correct
// by never having to represent it. This test pins all three; if any
// regresses, the graph's silence in the deleted view becomes a real
// hole and this fails.

import (
	"context"
	"errors"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/chain"
)

func TestChainLifecycle_LiveChildOnDeadParentIsUnreachable(t *testing.T) {
	ctx := context.Background()

	// --- Guard 1 (fix #15, leaf-first): you cannot soft-delete a
	// parent out from under a live child. This is what stops the
	// hazardous state from being created in the first place.
	t.Run("soft_delete_parent_refused_under_live_child", func(t *testing.T) {
		w := setupWorld(t)
		parent := w.commitWithChunks(t, "db1", "P", 0, "",
			backup.BackupTypeFull, 1, [][]byte{[]byte("p")})
		w.commitWithChunks(t, "db1", "C", 1, parent,
			backup.BackupTypeIncremental, 1, [][]byte{[]byte("c")})
		if err := w.store.SoftDelete(ctx, "db1", parent, "manual", "x"); err == nil {
			t.Fatal("soft-deleting a parent with a live incremental child SUCCEEDED — the " +
				"leaf-first guard is gone, and a live child can now sit on a tombstoned " +
				"parent that `backup graph --include-deleted` will show as connected")
		}
	})

	// --- Guard 2 (fix #16): once a parent IS tombstoned (only
	// reachable via cascade, which tombstones the child too), the
	// child cannot be individually resurrected back to live. So a
	// live child over a tombstoned parent can't be reached from the
	// other direction either.
	t.Run("undelete_child_refused_while_parent_tombstoned", func(t *testing.T) {
		w := setupWorld(t)
		parent := w.commitWithChunks(t, "db1", "P", 0, "",
			backup.BackupTypeFull, 1, [][]byte{[]byte("p")})
		child := w.commitWithChunks(t, "db1", "C", 1, parent,
			backup.BackupTypeIncremental, 1, [][]byte{[]byte("c")})
		// Cascade tombstones both, leaf-first — the ONLY way to
		// tombstone the parent at all.
		if _, err := w.store.SoftDeleteCascade(ctx, "db1", parent, "manual", "x"); err != nil {
			t.Fatalf("cascade delete: %v", err)
		}
		_, err := w.store.Undelete(ctx, "db1", child)
		var pt *backup.UndeleteParentTombstonedError
		if !errors.As(err, &pt) {
			t.Fatalf("undelete of a child under a tombstoned parent returned %v, want "+
				"UndeleteParentTombstonedError — without it the child could go live over a "+
				"tombstoned parent, the exact state the graph's deleted view can't flag", err)
		}
	})

	// --- Guard 3: the one route that DOES produce a live child with
	// an unavailable ancestor — the parent hard-deleted out of band
	// (or GC'd after grace) — is NOT tombstoned, so it is absent from
	// BOTH views, and the child is a genuine orphan flagged critical
	// in BOTH. Adding --include-deleted does not silence it.
	t.Run("hard_deleted_parent_orphan_flagged_in_both_views", func(t *testing.T) {
		w := setupWorld(t)
		orphanID := w.orphan(t, "db1") // commits parent+child, hard-deletes parent

		for _, incTomb := range []bool{false, true} {
			g, err := chain.BuildGraph(ctx, w.sp, "db1", chain.Options{
				Verifier:          w.verifier,
				IncludeTombstoned: incTomb,
			})
			if err != nil {
				t.Fatalf("BuildGraph(incTomb=%v): %v", incTomb, err)
			}
			if !hasCritical(g, orphanID) {
				t.Fatalf("orphan %q NOT flagged critical with IncludeTombstoned=%v — the "+
					"restorability warning must survive the deleted view.\nissues: %+v",
					orphanID, incTomb, g.Issues)
			}
		}
	})
}

func hasCritical(g *chain.Graph, backupID string) bool {
	for _, iss := range g.Issues {
		if iss.Severity == "critical" && iss.BackupID == backupID {
			return true
		}
	}
	return false
}
