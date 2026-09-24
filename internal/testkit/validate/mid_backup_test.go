package validate_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/config"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/inject"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/validate"
)

// timingCell records when its backups ran and when faults were applied.
type timingCell struct {
	*validate.FakeCellRuntime
	mu      sync.Mutex
	backups [][2]time.Time
	faults  []time.Time
}

func (c *timingCell) TakeBackup(ctx context.Context) (string, error) {
	start := time.Now()
	time.Sleep(40 * time.Millisecond)
	id, err := c.FakeCellRuntime.TakeBackup(ctx)
	c.mu.Lock()
	c.backups = append(c.backups, [2]time.Time{start, time.Now()})
	c.mu.Unlock()
	return id, err
}

func (c *timingCell) ApplyFault(ctx context.Context, action string) (inject.Recovery, error) {
	c.mu.Lock()
	c.faults = append(c.faults, time.Now())
	c.mu.Unlock()
	return inject.NoRecovery, nil
}

// drop_relation_mid_backup was applied in the fault step, which always
// finishes before the backup step starts: it never overlapped a backup.
// A *_mid_backup fault must land while a backup is running.
func TestRun_MidBackupFaultOverlapsABackup(t *testing.T) {
	defer validate.SetMidBackupDelayForTest(5 * time.Millisecond)()
	validate.ResetForTesting()
	cell := &timingCell{FakeCellRuntime: &validate.FakeCellRuntime{NameStr: "c"}}
	_, err := validate.Run(context.Background(), validate.RunOptions{
		Seed: 1, Duration: 300 * time.Millisecond,
		Loop: validate.LoopOptions{BackupEvery: 99, FaultProbability: 1,
			HealWindow: time.Microsecond, RetentionInterval: -1},
		Faults: &config.Faults{Schema: config.FaultSchema, Version: 1, Faults: []config.Fault{
			{Name: "drop", Weight: 1, Action: "drop_relation_mid_backup(target=pg_random)"}}},
		Cells: []validate.CellRuntime{cell},
	})
	if err != nil {
		t.Fatal(err)
	}
	cell.mu.Lock()
	defer cell.mu.Unlock()
	if len(cell.faults) == 0 {
		t.Fatal("no fault applied")
	}
	for _, f := range cell.faults {
		inside := false
		for _, b := range cell.backups {
			if f.After(b[0]) && f.Before(b[1]) {
				inside = true
			}
		}
		if !inside {
			t.Fatalf("a mid-backup fault was applied at %s, outside every backup %v", f.Format(time.StampMicro), cell.backups)
		}
	}
}
