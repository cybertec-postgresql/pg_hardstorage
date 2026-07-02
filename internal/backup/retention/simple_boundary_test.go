package retention_test

import (
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/retention"
)

// TestSimple_ExactBoundaryBackupKept pins the documented contract: a
// backup whose StoppedAt is EXACTLY KeepFor old is kept (the cut-off is
// inclusive, >= not >). The prior StoppedAt.After(cutoff) comparison
// deleted the exact-boundary backup, contradicting simple.go's type doc.
func TestSimple_ExactBoundaryBackupKept(t *testing.T) {
	keepFor := 24 * time.Hour

	// newest: safety-net keep (always kept regardless of age).
	newest := mk(refUTC)
	// boundary: StoppedAt == now-KeepFor exactly → must be KEPT.
	boundary := mk(refUTC.Add(-keepFor))
	// justOlder: one second past the boundary → must be DELETED.
	justOlder := mk(refUTC.Add(-keepFor - time.Second))

	in := []*backup.Manifest{newest, boundary, justOlder}
	d := retention.SimplePolicy{KeepFor: keepFor}.Apply(refUTC, in)

	kept := map[string]bool{}
	for _, m := range d.Keep {
		kept[m.BackupID] = true
	}

	if !kept[boundary.BackupID] {
		t.Errorf("backup exactly KeepFor old must be KEPT (inclusive boundary); it was deleted")
	}
	if kept[justOlder.BackupID] {
		t.Errorf("backup older than KeepFor must be deleted; it was kept")
	}
	if !kept[newest.BackupID] {
		t.Errorf("newest backup must always be kept (safety net)")
	}
	if d.KeptCount() != 2 {
		t.Errorf("kept = %d, want 2 (newest + exact-boundary)", d.KeptCount())
	}
}
