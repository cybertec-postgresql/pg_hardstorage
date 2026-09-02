package validate_test

// A fault that could not be reverted must be visible in the report and
// must not be reported as recovered.
//
// Two defects lived here together:
//
//  1. FaultStats.RecoveryFails was rendered by the soak report
//     ("- Recovery failures: %d") and incremented by nothing. Every soak
//     reported zero unrevertable faults no matter how many
//     recovery_failed events fired. The release gate reads that report.
//
//  2. The fault_recovered event was emitted unconditionally after the
//     revert attempt -- including when the revert had just failed. Since
//     fault_recovered is one of the three ops the watch TUI paints as
//     healthy, and it landed AFTER recovery_failed, a cell whose fault
//     could not be undone displayed green for the rest of the run while
//     sitting in the degraded state the fault created.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/validate"
)

func TestRun_UnrevertableFaultIsCountedAndNotCalledRecovered(t *testing.T) {
	validate.ResetForTesting()
	cell := &validate.FakeCellRuntime{
		NameStr:     "stuck-cell",
		RecoveryErr: errors.New("simulated: disk_full revert could not free the loopback file"),
	}
	emit, events, mu := collectEvents(t)

	rep, err := validate.Run(context.Background(), validate.RunOptions{
		Seed:     1,
		Duration: 200 * time.Millisecond,
		// FaultProbability 1 so every iteration applies a fault and the
		// revert failure is certain to fire.
		Loop:    validate.LoopOptions{BackupEvery: 99, FaultProbability: 1, HealWindow: time.Millisecond},
		Faults:  defaultFaults(),
		Cells:   []validate.CellRuntime{cell},
		OnEvent: emit,
	})
	if err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	var failed, recovered int
	var lastFaultOp string
	for _, ev := range *events {
		switch ev.Op {
		case "recovery_failed":
			failed++
			lastFaultOp = ev.Op
		case "fault_recovered":
			recovered++
			lastFaultOp = ev.Op
		}
	}
	mu.Unlock()

	if failed == 0 {
		t.Fatal("no recovery_failed events; the fixture did not exercise a failing revert")
	}
	if recovered != 0 {
		t.Errorf("fault_recovered emitted %d time(s) for a cell whose every revert failed.\n\n"+
			"fault_recovered is painted as a healthy LastOp by the watch TUI, so emitting it "+
			"behind recovery_failed makes a poisoned cell read green.", recovered)
	}
	if lastFaultOp == "fault_recovered" {
		t.Errorf("the LAST fault-lifecycle event was fault_recovered after a failed revert; " +
			"that is precisely what the TUI shows the operator")
	}

	if rep.FaultStats.RecoveryFails != failed {
		t.Errorf("FaultStats.RecoveryFails = %d, want %d (one per recovery_failed event).\n\n"+
			"The report renders this as \"- Recovery failures: %%d\". A zero here tells a "+
			"release engineer that every injected fault was cleanly reverted.",
			rep.FaultStats.RecoveryFails, failed)
	}
	var cellFails int
	for _, c := range rep.Cells {
		cellFails += c.RecoveryFails
	}
	if cellFails != rep.FaultStats.RecoveryFails {
		t.Errorf("per-cell RecoveryFails sum to %d but the rollup says %d",
			cellFails, rep.FaultStats.RecoveryFails)
	}
}

// The healthy path must be unchanged: a revert that succeeds still
// reports fault_recovered and contributes nothing to RecoveryFails.
func TestRun_RevertibleFaultStillReportsRecovered(t *testing.T) {
	validate.ResetForTesting()
	cell := &validate.FakeCellRuntime{NameStr: "healthy-cell"}
	emit, events, mu := collectEvents(t)

	rep, err := validate.Run(context.Background(), validate.RunOptions{
		Seed:     1,
		Duration: 200 * time.Millisecond,
		Loop:     validate.LoopOptions{BackupEvery: 99, FaultProbability: 1, HealWindow: time.Millisecond},
		Faults:   defaultFaults(),
		Cells:    []validate.CellRuntime{cell},
		OnEvent:  emit,
	})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	var recovered, failed int
	for _, ev := range *events {
		switch ev.Op {
		case "fault_recovered":
			recovered++
		case "recovery_failed":
			failed++
		}
	}
	mu.Unlock()
	if recovered == 0 {
		t.Error("a cell whose reverts all succeed emitted no fault_recovered event")
	}
	if failed != 0 {
		t.Errorf("recovery_failed emitted %d time(s) on a healthy cell", failed)
	}
	if rep.FaultStats.RecoveryFails != 0 {
		t.Errorf("RecoveryFails = %d on a healthy run", rep.FaultStats.RecoveryFails)
	}
}
