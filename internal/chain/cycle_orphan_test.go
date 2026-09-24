package chain

import "testing"

// A parent_backup_id cycle was reported as chain.cycle_detected AND the
// broken member as chain.orphaned_incremental "references parent … not
// present in the visible set" — the parent IS present; it is part of the
// cycle — with every cycle member stuck at Depth 0. A cycle must be
// reported as a cycle only, and the rest of the chain must still get
// depths.
func TestLinkNodes_CycleIsNotAnOrphan(t *testing.T) {
	g := &Graph{}
	byID := map[string]*Node{
		"a": {BackupID: "a", Type: "incremental", ParentBackupID: "b"},
		"b": {BackupID: "b", Type: "incremental", ParentBackupID: "a"},
		"c": {BackupID: "c", Type: "incremental", ParentBackupID: "b"},
	}
	linkNodes(g, byID)

	codes := map[string]int{}
	for _, is := range g.Issues {
		codes[is.Code]++
	}
	if codes["chain.cycle_detected"] != 1 {
		t.Fatalf("cycle not reported exactly once: %+v", g.Issues)
	}
	if codes["chain.orphaned_incremental"] != 0 || g.OrphanCount != 0 {
		t.Fatalf("cycle member misreported as an orphan with a missing parent: %+v", g.Issues)
	}
	for id, n := range byID {
		if n.Depth == 0 {
			t.Errorf("node %s left at Depth 0", id)
		}
	}
}
