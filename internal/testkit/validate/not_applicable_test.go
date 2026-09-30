package validate_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/inject"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/validate"
)

// A fault that cannot produce its effect on the cell is a skip: it is
// neither applied (it did nothing) nor an apply failure.
func TestRun_FaultNotApplicableIsASkip(t *testing.T) {
	cell := &validate.FakeCellRuntime{NameStr: "big-disk",
		FaultErr: fmt.Errorf("disk_full: cap too small: %w", inject.ErrNotApplicable)}
	rep, evs := runOne(t, cell, 60*time.Millisecond, validate.LoopOptions{
		BackupEvery: 1, FaultProbability: 1, HealWindow: time.Microsecond})
	if countOp(evs, "fault_skipped_not_applicable") == 0 {
		t.Fatal("expected fault_skipped_not_applicable")
	}
	if n := countOp(evs, "fault_apply_failed"); n != 0 {
		t.Errorf("%d fault_apply_failed for a fault that was merely not applicable", n)
	}
	if rep.Cells[0].FaultsApplied != 0 {
		t.Errorf("FaultsApplied = %d for faults that never applied", rep.Cells[0].FaultsApplied)
	}
}
