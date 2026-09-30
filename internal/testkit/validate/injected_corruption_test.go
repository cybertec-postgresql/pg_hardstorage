package validate_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/config"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/inject"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/report"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/validate"
)

// damagedRepoCell refuses to restore a backup that a fault was applied
// after — what pg_hardstorage does when a corruption fault hit the
// backup's manifest or WAL.
type damagedRepoCell struct {
	*validate.FakeCellRuntime
	mu         sync.Mutex
	lastBackup time.Time
	lastFault  time.Time
}

func (c *damagedRepoCell) TakeBackup(ctx context.Context) (string, error) {
	id, err := c.FakeCellRuntime.TakeBackup(ctx)
	c.mu.Lock()
	c.lastBackup = time.Now()
	c.mu.Unlock()
	time.Sleep(time.Millisecond)
	return id, err
}

func (c *damagedRepoCell) ApplyFault(ctx context.Context, action string) (inject.Recovery, error) {
	c.mu.Lock()
	c.lastFault = time.Now()
	c.mu.Unlock()
	time.Sleep(time.Millisecond)
	return inject.NoRecovery, nil
}

func (c *damagedRepoCell) VerifyRestore(context.Context, string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lastFault.After(c.lastBackup) {
		return errors.New(`restore: {"code":"verify.manifest_hash_mismatch"}`)
	}
	return nil
}

func runDamaged(t *testing.T, action string) (*report.Report, []validate.Event) {
	t.Helper()
	validate.ResetForTesting()
	emit, events, mu := collectEvents(t)
	cell := &damagedRepoCell{FakeCellRuntime: &validate.FakeCellRuntime{NameStr: "c"}}
	rep, err := validate.Run(context.Background(), validate.RunOptions{
		Seed: 1, Duration: 150 * time.Millisecond,
		Loop: validate.LoopOptions{BackupEvery: 3, VerifyEvery: 1, FaultProbability: 1,
			HealWindow: time.Microsecond, RetentionInterval: -1},
		Faults: &config.Faults{Schema: config.FaultSchema, Version: 1,
			Faults: []config.Fault{{Name: "f", Weight: 1, Action: action}}},
		Cells: []validate.CellRuntime{cell}, OnEvent: emit,
	})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	return rep, append([]validate.Event(nil), (*events)...)
}

// A restore that refuses a backup the cell's own corruption fault
// damaged is the product detecting the damage — the success signal —
// not a product failure.
func TestRun_DetectedInjectedCorruptionIsNotAFailure(t *testing.T) {
	rep, evs := runDamaged(t, "missing_wal_segment(target=repo)")
	if countOp(evs, "verify_refused_injected_corruption") == 0 {
		t.Fatal("expected verify_refused_injected_corruption")
	}
	if !rep.OverallPass {
		t.Fatalf("detected injected corruption failed the run: %+v", rep.Failures)
	}
	if rep.Cells[0].CorruptionDetected == 0 {
		t.Error("detections must be counted")
	}
}

// Only after a corruption fault: the same refusal after a fault that
// damages nothing is a real verify failure.
func TestRun_VerifyFailureAfterOtherFaultStillFails(t *testing.T) {
	rep, _ := runDamaged(t, "signal(target=pg, sig=9)")
	if rep.OverallPass {
		t.Fatal("a verify failure after a non-corrupting fault must fail the run")
	}
}
