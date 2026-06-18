package validate_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/config"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/inject"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/report"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/validate"
)

func defaultFaults() *config.Faults {
	return &config.Faults{
		Schema: config.FaultSchema, Version: 1,
		Faults: []config.Fault{
			{Name: "ksig", Weight: 5, Action: "signal(target=agent, sig=15)"},
			{Name: "df", Weight: 3, Action: "disk_full(target=repo, fill=98%)"},
		},
	}
}

func collectEvents(t *testing.T) (func(validate.Event), *[]validate.Event, *sync.Mutex) {
	t.Helper()
	var mu sync.Mutex
	events := []validate.Event{}
	emit := func(ev validate.Event) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	}
	return emit, &events, &mu
}

func TestRun_SeedInvokedAfterSetup(t *testing.T) {
	validate.ResetForTesting()
	cell := &validate.FakeCellRuntime{NameStr: "seed-cell"}
	emit, events, mu := collectEvents(t)

	rep, err := validate.Run(context.Background(), validate.RunOptions{
		Seed:     1,
		Duration: 30 * time.Millisecond,
		Loop:     validate.LoopOptions{BackupEvery: 99, FaultProbability: 0},
		Faults:   defaultFaults(),
		Cells:    []validate.CellRuntime{cell},
		OnEvent:  emit,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OverallPass {
		t.Fatalf("expected pass; got %+v", rep.Failures)
	}
	calls, _ := cell.SeedCalls()
	if calls != 1 {
		t.Errorf("expected exactly 1 Seed call; got %d", calls)
	}
	mu.Lock()
	defer mu.Unlock()
	var sawSetupOK, sawSeedStarted, sawSeedOK bool
	for _, ev := range *events {
		switch ev.Op {
		case "setup_ok":
			sawSetupOK = true
		case "seed_started":
			if !sawSetupOK {
				t.Errorf("seed_started fired before setup_ok")
			}
			sawSeedStarted = true
		case "seed_ok":
			sawSeedOK = true
		}
	}
	if !sawSeedStarted || !sawSeedOK {
		t.Errorf("expected seed_started + seed_ok events; got started=%v ok=%v", sawSeedStarted, sawSeedOK)
	}
}

func TestRun_SeedFailureMarksReport(t *testing.T) {
	validate.ResetForTesting()
	cell := &validate.FakeCellRuntime{
		NameStr: "seed-fail",
		SeedErr: errors.New("simulated pgbench OOM"),
	}
	rep, err := validate.Run(context.Background(), validate.RunOptions{
		Seed:     1,
		Duration: 50 * time.Millisecond,
		Loop:     validate.LoopOptions{BackupEvery: 99, FaultProbability: 0},
		Faults:   defaultFaults(),
		Cells:    []validate.CellRuntime{cell},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.OverallPass {
		t.Errorf("expected fail")
	}
	if len(rep.Failures) == 0 || rep.Failures[0].Kind != "seed" {
		t.Errorf("expected first failure with kind=seed; got %+v", rep.Failures)
	}
}

func TestRun_SidecarsStartAndStop(t *testing.T) {
	validate.ResetForTesting()
	cell := &validate.FakeCellRuntime{NameStr: "sidecar-cell"}

	rep, err := validate.Run(context.Background(), validate.RunOptions{
		Seed:     1,
		Duration: 30 * time.Millisecond,
		Loop:     validate.LoopOptions{BackupEvery: 99, FaultProbability: 0},
		Faults:   defaultFaults(),
		Cells:    []validate.CellRuntime{cell},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OverallPass {
		t.Fatalf("expected pass; got %+v", rep.Failures)
	}
	startSus, stopSus, startWAL, stopWAL := cell.SidecarCalls()
	if startSus != 1 || stopSus != 1 {
		t.Errorf("sustained sidecar lifecycle off: start=%d stop=%d (want 1/1)", startSus, stopSus)
	}
	if startWAL != 1 || stopWAL != 1 {
		t.Errorf("wal-stream sidecar lifecycle off: start=%d stop=%d (want 1/1)", startWAL, stopWAL)
	}
}

func TestRun_LoadStatsMergedIntoCellReport(t *testing.T) {
	validate.ResetForTesting()
	statsFromSustained := &report.LoadStats{TPSAvg: 1234, LatencyP95Ms: 5.5, WALBytesWritten: 1024 * 1024 * 100, SustainedWriterRan: true}
	statsFromWALStream := &report.LoadStats{WALStreamLagBytes: 2048, WALStreamRan: true}
	cell := &validate.FakeCellRuntime{
		NameStr:        "metric-cell",
		SustainedStats: statsFromSustained,
		WALStreamStats: statsFromWALStream,
	}

	rep, err := validate.Run(context.Background(), validate.RunOptions{
		Seed:     1,
		Duration: 30 * time.Millisecond,
		Loop:     validate.LoopOptions{BackupEvery: 99, FaultProbability: 0},
		Faults:   defaultFaults(),
		Cells:    []validate.CellRuntime{cell},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OverallPass {
		t.Fatalf("expected pass; got %+v", rep.Failures)
	}
	if got := rep.Cells[0].LoadStats; got == nil {
		t.Fatal("expected LoadStats on cell report; got nil")
	} else {
		if got.TPSAvg != 1234 {
			t.Errorf("TPSAvg: got %v, want 1234", got.TPSAvg)
		}
		if got.LatencyP95Ms != 5.5 {
			t.Errorf("LatencyP95Ms: got %v, want 5.5", got.LatencyP95Ms)
		}
		if got.WALStreamLagBytes != 2048 {
			t.Errorf("WALStreamLagBytes: got %v, want 2048", got.WALStreamLagBytes)
		}
		if !got.SustainedWriterRan || !got.WALStreamRan {
			t.Errorf("expected both ran flags; got %+v", got)
		}
	}
}

func TestRun_DurationBoundsLoop(t *testing.T) {
	validate.ResetForTesting()
	cell := &validate.FakeCellRuntime{NameStr: "u24-pg17"}
	emit, events, mu := collectEvents(t)

	rep, err := validate.Run(context.Background(), validate.RunOptions{
		Project:  "test",
		Seed:     7,
		Duration: 60 * time.Millisecond,
		Loop: validate.LoopOptions{
			IterationInterval: 5 * time.Millisecond,
			BackupEvery:       3,
			VerifyEvery:       6,
			FaultProbability:  0,
		},
		Faults:  defaultFaults(),
		Cells:   []validate.CellRuntime{cell},
		OnEvent: emit,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OverallPass {
		t.Errorf("expected pass; got fail: %+v", rep.Failures)
	}
	if len(rep.Cells) != 1 {
		t.Fatalf("expected 1 cell report; got %d", len(rep.Cells))
	}
	if rep.Cells[0].IterationsRun < 2 {
		t.Errorf("expected ≥2 iterations in 60ms; got %d", rep.Cells[0].IterationsRun)
	}
	mu.Lock()
	gotDurationEvent := false
	for _, ev := range *events {
		if ev.Op == "duration_elapsed" {
			gotDurationEvent = true
		}
	}
	mu.Unlock()
	if !gotDurationEvent {
		t.Errorf("expected duration_elapsed event")
	}
}

func TestRun_BackupAndVerifyCadence(t *testing.T) {
	validate.ResetForTesting()
	cell := &validate.FakeCellRuntime{NameStr: "u24-pg17"}

	rep, err := validate.Run(context.Background(), validate.RunOptions{
		Seed:     7,
		Duration: 200 * time.Millisecond,
		Loop: validate.LoopOptions{
			BackupEvery:      2,
			VerifyEvery:      4,
			FaultProbability: 0,
		},
		Faults: defaultFaults(),
		Cells:  []validate.CellRuntime{cell},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, backupCalls, verifyCalls, _, _ := cell.Calls()
	if rep.Cells[0].BackupsTaken != backupCalls {
		t.Errorf("BackupsTaken=%d but FakeCellRuntime saw %d calls",
			rep.Cells[0].BackupsTaken, backupCalls)
	}
	// Verify cadence is half of backup cadence in our config —
	// expect the report to reflect that.
	if verifyCalls > backupCalls {
		t.Errorf("verifyCalls (%d) should not exceed backupCalls (%d)",
			verifyCalls, backupCalls)
	}
}

func TestRun_BackupFailureMarksReport(t *testing.T) {
	validate.ResetForTesting()
	cell := &validate.FakeCellRuntime{
		NameStr:   "rocky9",
		BackupErr: errors.New("dd: out of disk"),
	}
	rep, err := validate.Run(context.Background(), validate.RunOptions{
		Seed:     1,
		Duration: 100 * time.Millisecond,
		Loop:     validate.LoopOptions{BackupEvery: 1, FaultProbability: 0},
		Faults:   defaultFaults(),
		Cells:    []validate.CellRuntime{cell},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.OverallPass {
		t.Errorf("expected fail")
	}
	if len(rep.Failures) == 0 {
		t.Fatal("expected at least one Failure")
	}
	if rep.Failures[0].Cell != "rocky9" || rep.Failures[0].Kind != "backup" {
		t.Errorf("first failure shape wrong: %+v", rep.Failures[0])
	}
	if rep.Cells[0].Pass {
		t.Errorf("cell should be marked failed")
	}
}

func TestRun_VerifyFailureMarksReport(t *testing.T) {
	validate.ResetForTesting()
	cell := &validate.FakeCellRuntime{
		NameStr:      "u24-verify",
		VerifyAfterN: 1,
	}
	rep, err := validate.Run(context.Background(), validate.RunOptions{
		Seed:     1,
		Duration: 200 * time.Millisecond,
		Loop:     validate.LoopOptions{BackupEvery: 1, VerifyEvery: 2, FaultProbability: 0},
		Faults:   defaultFaults(),
		Cells:    []validate.CellRuntime{cell},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.OverallPass {
		t.Errorf("expected fail")
	}
	hasVerify := false
	for _, f := range rep.Failures {
		if f.Kind == "verify" {
			hasVerify = true
		}
	}
	if !hasVerify {
		t.Errorf("expected verify failure in report.Failures: %+v", rep.Failures)
	}
}

// TestRun_VerifyAbortedAtDeadlineNotReportedAsFailure pins the
// fix for the verify-at-deadline regression surfaced by the
// 10-min endurance soak: when the run-wide context fires
// mid-verify, the docker exec subprocess gets SIGKILL'd (exit
// 137, empty output) and the old code reported that as a real
// verify failure — 17 cells across 4 slots failed this way on
// the first 10-min run.  The orchestrator now mirrors the
// backup_aborted_at_deadline path: detect ctx.Err() != nil,
// emit verify_aborted_at_deadline, return cleanly, leave
// rep.OverallPass intact.
func TestRun_VerifyAbortedAtDeadlineNotReportedAsFailure(t *testing.T) {
	validate.ResetForTesting()
	cell := &validate.FakeCellRuntime{
		NameStr:              "u24-verify-deadline",
		VerifyBlocksUntilCtx: true,
	}
	rep, err := validate.Run(context.Background(), validate.RunOptions{
		Seed:     1,
		Duration: 100 * time.Millisecond,
		Loop: validate.LoopOptions{
			BackupEvery: 1, VerifyEvery: 1, FaultProbability: 0,
		},
		Faults: defaultFaults(),
		Cells:  []validate.CellRuntime{cell},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OverallPass {
		t.Errorf("deadline-cancelled verify must NOT fail the run; got failures: %+v", rep.Failures)
	}
	for _, f := range rep.Failures {
		if f.Kind == "verify" {
			t.Errorf("deadline-cancelled verify should not appear in report.Failures: %+v", f)
		}
	}
}

func TestRun_FaultProbabilityZeroSkipsFaults(t *testing.T) {
	validate.ResetForTesting()
	cell := &validate.FakeCellRuntime{NameStr: "x"}
	_, err := validate.Run(context.Background(), validate.RunOptions{
		Seed:     1,
		Duration: 100 * time.Millisecond,
		Loop:     validate.LoopOptions{BackupEvery: 99, FaultProbability: 0},
		Faults:   defaultFaults(),
		Cells:    []validate.CellRuntime{cell},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, _, faultCalls, _ := cell.Calls()
	if faultCalls != 0 {
		t.Errorf("expected zero fault calls with probability=0; got %d", faultCalls)
	}
}

func TestRun_FaultProbabilityOneAlwaysFires(t *testing.T) {
	validate.ResetForTesting()
	cell := &validate.FakeCellRuntime{NameStr: "y"}
	_, err := validate.Run(context.Background(), validate.RunOptions{
		Seed:     1,
		Duration: 200 * time.Millisecond,
		Loop: validate.LoopOptions{
			IterationInterval: 5 * time.Millisecond,
			BackupEvery:       99,
			FaultProbability:  1.0,
			HealWindow:        time.Microsecond,
		},
		Faults: defaultFaults(),
		Cells:  []validate.CellRuntime{cell},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, _, faultCalls, _ := cell.Calls()
	if faultCalls == 0 {
		t.Errorf("expected ≥1 fault call with probability=1; got 0")
	}
}

// TestRun_FaultOnDownCell_RecordsSkipNotFailure locks the fix for the
// inconsistency the 2026-05-23 batched soak surfaced: a fault firing on
// a cell a prior fault already downed (disk_full / pause_archive returning
// inject.ErrTargetNotRunning) is a benign race and must be recorded as
// fault_skipped_cell_down, NOT fault_apply_failed (which the soak triages
// as a real failure).
func TestRun_FaultOnDownCell_RecordsSkipNotFailure(t *testing.T) {
	validate.ResetForTesting()
	emit, events, mu := collectEvents(t)
	cell := &validate.FakeCellRuntime{
		NameStr:  "down",
		FaultErr: fmt.Errorf("disk_full: df down: %w", inject.ErrTargetNotRunning),
	}
	_, err := validate.Run(context.Background(), validate.RunOptions{
		Seed:     1,
		Duration: 150 * time.Millisecond,
		Loop: validate.LoopOptions{
			IterationInterval: 5 * time.Millisecond,
			BackupEvery:       99,
			FaultProbability:  1.0,
			HealWindow:        time.Microsecond,
		},
		Faults:  defaultFaults(),
		Cells:   []validate.CellRuntime{cell},
		OnEvent: emit,
	})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	var sawSkip, sawFailed bool
	for _, ev := range *events {
		switch ev.Op {
		case "fault_skipped_cell_down":
			sawSkip = true
		case "fault_apply_failed":
			sawFailed = true
		}
	}
	if !sawSkip {
		t.Errorf("expected fault_skipped_cell_down for ErrTargetNotRunning")
	}
	if sawFailed {
		t.Errorf("ErrTargetNotRunning must NOT be recorded as fault_apply_failed")
	}
}

// TestRun_FaultGenericError_RecordsFailure is the control: a non-sentinel
// fault error is still reported as fault_apply_failed.
func TestRun_FaultGenericError_RecordsFailure(t *testing.T) {
	validate.ResetForTesting()
	emit, events, mu := collectEvents(t)
	cell := &validate.FakeCellRuntime{
		NameStr:  "broken-fault",
		FaultErr: errors.New("disk_full: dd: no space left on host"),
	}
	_, err := validate.Run(context.Background(), validate.RunOptions{
		Seed:     1,
		Duration: 150 * time.Millisecond,
		Loop: validate.LoopOptions{
			IterationInterval: 5 * time.Millisecond,
			BackupEvery:       99,
			FaultProbability:  1.0,
			HealWindow:        time.Microsecond,
		},
		Faults:  defaultFaults(),
		Cells:   []validate.CellRuntime{cell},
		OnEvent: emit,
	})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	var sawFailed, sawSkip bool
	for _, ev := range *events {
		switch ev.Op {
		case "fault_apply_failed":
			sawFailed = true
		case "fault_skipped_cell_down":
			sawSkip = true
		}
	}
	if !sawFailed {
		t.Errorf("expected fault_apply_failed for a generic fault error")
	}
	if sawSkip {
		t.Errorf("generic error must NOT be recorded as fault_skipped_cell_down")
	}
}

func TestRun_SetupFailureSkipsLoop(t *testing.T) {
	validate.ResetForTesting()
	cell := &validate.FakeCellRuntime{
		NameStr:  "broken",
		SetupErr: errors.New("PG unreachable"),
	}
	rep, err := validate.Run(context.Background(), validate.RunOptions{
		Seed: 1, Duration: 100 * time.Millisecond,
		Faults: defaultFaults(),
		Cells:  []validate.CellRuntime{cell},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.OverallPass {
		t.Errorf("expected fail when setup errors")
	}
	_, drive, backup, _, _, teardown := cell.Calls()
	if drive != 0 || backup != 0 {
		t.Errorf("setup-failure cell should not Drive or Backup; got drive=%d backup=%d", drive, backup)
	}
	if teardown != 0 {
		t.Errorf("setup-failure cell should not Teardown; got %d", teardown)
	}
}

func TestRun_ParallelCells(t *testing.T) {
	validate.ResetForTesting()
	const N = 5
	cells := make([]validate.CellRuntime, N)
	fakes := make([]*validate.FakeCellRuntime, N)
	for i := 0; i < N; i++ {
		fakes[i] = &validate.FakeCellRuntime{NameStr: fmt.Sprintf("c-%d", i)}
		cells[i] = fakes[i]
	}
	rep, err := validate.Run(context.Background(), validate.RunOptions{
		Seed:     7,
		Duration: 80 * time.Millisecond,
		Loop:     validate.LoopOptions{BackupEvery: 99, FaultProbability: 0},
		Faults:   defaultFaults(),
		Cells:    cells,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Cells) != N {
		t.Errorf("expected %d cell reports; got %d", N, len(rep.Cells))
	}
	for i, f := range fakes {
		s, _, _, _, _, td := f.Calls()
		if s != 1 || td != 1 {
			t.Errorf("cell %d: expected 1 setup + 1 teardown; got s=%d td=%d", i, s, td)
		}
	}
}

// TestRun_SetupConcurrencyCapsParallelSetup is the regression
// for the parallel-soak failure where 30 cells × 8 slots all
// called Setup() at once and starved every PG of CPU during
// initdb (waitForPG hit "connection reset" because PG hadn't
// finished initdb in 90s).  The orchestrator's setup
// semaphore caps concurrent Setup() calls at
// RunOptions.SetupConcurrency.
func TestRun_SetupConcurrencyCapsParallelSetup(t *testing.T) {
	validate.ResetForTesting()
	const (
		N             = 12
		concurrency   = 3
		setupHoldTime = 20 * time.Millisecond
	)
	var (
		mu          sync.Mutex
		inFlight    int
		maxInFlight int
	)
	hook := func(_ context.Context, _ string) error {
		mu.Lock()
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		mu.Unlock()
		// Hold the slot long enough that, without the
		// semaphore, all N cells would be in Setup() at once.
		time.Sleep(setupHoldTime)
		mu.Lock()
		inFlight--
		mu.Unlock()
		return nil
	}
	cells := make([]validate.CellRuntime, N)
	for i := 0; i < N; i++ {
		cells[i] = &validate.FakeCellRuntime{
			NameStr:   fmt.Sprintf("c-%d", i),
			SetupFunc: hook,
		}
	}
	if _, err := validate.Run(context.Background(), validate.RunOptions{
		Seed:             1,
		Duration:         500 * time.Millisecond,
		Loop:             validate.LoopOptions{BackupEvery: 99, FaultProbability: 0},
		Faults:           defaultFaults(),
		Cells:            cells,
		SetupConcurrency: concurrency,
	}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if maxInFlight > concurrency {
		t.Errorf("max concurrent Setup() = %d; should be capped at %d (semaphore broken)",
			maxInFlight, concurrency)
	}
	if maxInFlight < 1 {
		t.Errorf("max concurrent Setup() = %d; expected at least 1 (test didn't run)",
			maxInFlight)
	}
}

func TestRun_DeterministicFaultPicksSameSeed(t *testing.T) {
	validate.ResetForTesting()
	pickFor := func(seed int64) []string {
		var picks []string
		var mu sync.Mutex
		cell := &validate.FakeCellRuntime{NameStr: "x"}
		_, err := validate.Run(context.Background(), validate.RunOptions{
			Seed:     seed,
			Duration: 200 * time.Millisecond,
			Loop: validate.LoopOptions{
				IterationInterval: 5 * time.Millisecond,
				BackupEvery:       99,
				FaultProbability:  1.0,
				HealWindow:        time.Microsecond,
			},
			Faults: defaultFaults(),
			Cells:  []validate.CellRuntime{cell},
			OnEvent: func(ev validate.Event) {
				if ev.Op == "fault_apply" {
					mu.Lock()
					picks = append(picks, ev.Detail)
					mu.Unlock()
				}
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return picks
	}
	picks1 := pickFor(42)
	validate.ResetForTesting()
	picks2 := pickFor(42)
	// Run lengths can differ by ±1 due to wall-clock vs ctx
	// deadline race; the rng-determinism contract is on the
	// COMMON PREFIX of picks, not the total length.
	common := len(picks1)
	if len(picks2) < common {
		common = len(picks2)
	}
	if common == 0 {
		t.Fatalf("seed=42 produced no faults in either run; loop too short")
	}
	for i := 0; i < common; i++ {
		if picks1[i] != picks2[i] {
			t.Errorf("seed=42 picks differ at index %d: %q vs %q",
				i, picks1[i], picks2[i])
		}
	}
}

func TestRun_NoCells_Errors(t *testing.T) {
	validate.ResetForTesting()
	_, err := validate.Run(context.Background(), validate.RunOptions{
		Duration: time.Millisecond,
		Faults:   defaultFaults(),
	})
	if err == nil || !strings.Contains(err.Error(), "no cells") {
		t.Errorf("expected no-cells error; got %v", err)
	}
}
