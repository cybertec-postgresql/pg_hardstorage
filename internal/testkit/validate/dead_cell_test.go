package validate_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/inject"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/report"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/validate"
)

func runOne(t *testing.T, cell validate.CellRuntime, d time.Duration, loop validate.LoopOptions) (*report.Report, []validate.Event) {
	t.Helper()
	validate.ResetForTesting()
	emit, events, mu := collectEvents(t)
	if loop.RetentionInterval == 0 {
		loop.RetentionInterval = -1
	}
	rep, err := validate.Run(context.Background(), validate.RunOptions{
		Seed: 1, Duration: d, Loop: loop, Faults: defaultFaults(),
		Cells: []validate.CellRuntime{cell}, OnEvent: emit,
	})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	return rep, append([]validate.Event(nil), (*events)...)
}

func failureKinds(rep *report.Report) []string {
	var k []string
	for _, f := range rep.Failures {
		k = append(k, f.Kind)
	}
	return k
}

// A cell that a fault took down for good skipped every later backup as
// backup_skipped_cell_down — and a run of nothing but skips PASSED. The
// soak's verdict must not be satisfiable by a cell that never produced
// a single backup.
func TestRun_CellThatNeverBacksUpFails(t *testing.T) {
	cell := &validate.FakeCellRuntime{NameStr: "dead",
		BackupErr: fmt.Errorf("fault downed it: %w", validate.ErrCellNotReady)}
	rep, evs := runOne(t, cell, 100*time.Millisecond, validate.LoopOptions{BackupEvery: 1})
	if countOp(evs, "backup_skipped_cell_down") == 0 {
		t.Fatal("fixture produced no skipped backups")
	}
	if rep.OverallPass {
		t.Fatal("a cell with zero successful backups over the whole run passed")
	}
	if k := failureKinds(rep); len(k) == 0 || k[0] != "cell_down" {
		t.Errorf("failure kinds = %v, want cell_down", k)
	}
}

// flakyCell backs up fine until it dies.
type flakyCell struct {
	*validate.FakeCellRuntime
	okBackups int32
	n         atomic.Int32
}

func (f *flakyCell) TakeBackup(ctx context.Context) (string, error) {
	if f.n.Add(1) > f.okBackups {
		return "", validate.ErrCellNotReady
	}
	return f.FakeCellRuntime.TakeBackup(ctx)
}

// A cell that dies mid-run is caught once it has gone MaxBackupGap
// without a backup, not only when it never produced one.
func TestRun_CellDownLongerThanTheBoundFails(t *testing.T) {
	cell := &flakyCell{FakeCellRuntime: &validate.FakeCellRuntime{NameStr: "dies"}, okBackups: 2}
	rep, evs := runOne(t, cell, 300*time.Millisecond, validate.LoopOptions{
		BackupEvery: 1, IterationInterval: time.Millisecond, MaxBackupGap: 50 * time.Millisecond})
	if countOp(evs, "backup_completed") != 2 {
		t.Fatalf("fixture: %d backups completed, want 2", countOp(evs, "backup_completed"))
	}
	if rep.OverallPass {
		t.Fatal("a cell down for longer than MaxBackupGap passed")
	}
	if k := failureKinds(rep); len(k) == 0 || k[0] != "cell_down" {
		t.Errorf("failure kinds = %v, want cell_down", k)
	}
}

// The control: skips that the cell recovers from within the bound are
// the testbed doing its job and are not failures.
func TestRun_BriefOutagesDoNotFail(t *testing.T) {
	cell := &skipEveryOther{FakeCellRuntime: &validate.FakeCellRuntime{NameStr: "brief"}}
	rep, evs := runOne(t, cell, 150*time.Millisecond, validate.LoopOptions{
		BackupEvery: 1, IterationInterval: time.Millisecond, MaxBackupGap: time.Minute})
	if countOp(evs, "backup_skipped_cell_down") == 0 || countOp(evs, "backup_completed") == 0 {
		t.Fatal("fixture must both skip and complete backups")
	}
	if !rep.OverallPass {
		t.Fatalf("brief outages failed the run: %+v", rep.Failures)
	}
}

type skipEveryOther struct {
	*validate.FakeCellRuntime
	n atomic.Int32
}

func (s *skipEveryOther) TakeBackup(ctx context.Context) (string, error) {
	if s.n.Add(1)%2 == 0 {
		return "", validate.ErrCellNotReady
	}
	return s.FakeCellRuntime.TakeBackup(ctx)
}

// A fault that could not be reverted leaves the cell in whatever state
// the fault created, for the rest of the run: nothing measured on it
// afterwards can be trusted, so the run must not pass. It is recorded
// as a "recovery" failure — the testbed's, not the product's.
func TestRun_FailedRecoveryFailsTheRun(t *testing.T) {
	cell := &validate.FakeCellRuntime{NameStr: "stuck",
		RecoveryErr: errors.New("docker start: container stays down")}
	rep, evs := runOne(t, cell, 100*time.Millisecond, validate.LoopOptions{
		BackupEvery: 99, FaultProbability: 1, HealWindow: time.Millisecond})
	if countOp(evs, "recovery_failed") == 0 {
		t.Fatal("fixture produced no recovery_failed")
	}
	if rep.OverallPass {
		t.Fatal("a run with an unrevertable fault passed")
	}
	if k := failureKinds(rep); len(k) == 0 || k[0] != "recovery" {
		t.Errorf("failure kinds = %v, want recovery", k)
	}
}

// ctxRecoveryCell's revert honours its context, as every docker-backed
// recovery does.
type ctxRecoveryCell struct {
	*validate.FakeCellRuntime
	reverted atomic.Bool
}

func (c *ctxRecoveryCell) ApplyFault(ctx context.Context, action string) (inject.Recovery, error) {
	if _, err := c.FakeCellRuntime.ApplyFault(ctx, action); err != nil {
		return nil, err
	}
	return func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		c.reverted.Store(true)
		return nil
	}, nil
}

// At the run deadline the heal window ends early and the fault is
// reverted — with the run's context, already cancelled, so every
// docker call of the revert failed at once and the fault stayed
// applied through teardown (reported only as recovery_aborted_at_
// deadline). The revert needs a context of its own.
func TestRun_RecoveryAtDeadlineStillReverts(t *testing.T) {
	cell := &ctxRecoveryCell{FakeCellRuntime: &validate.FakeCellRuntime{NameStr: "late"}}
	rep, evs := runOne(t, cell, 80*time.Millisecond, validate.LoopOptions{
		BackupEvery: 99, FaultProbability: 1, HealWindow: time.Hour})
	if countOp(evs, "fault_apply") == 0 {
		t.Fatal("fixture applied no fault")
	}
	if !cell.reverted.Load() {
		t.Fatal("the fault in flight at the deadline was never reverted")
	}
	if countOp(evs, "fault_recovered") == 0 || !rep.OverallPass {
		t.Errorf("a revert that succeeded at the deadline must read as recovered: pass=%v failures=%+v",
			rep.OverallPass, rep.Failures)
	}
}
