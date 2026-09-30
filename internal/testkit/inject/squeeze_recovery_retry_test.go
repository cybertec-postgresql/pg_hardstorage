package inject

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func fastSqueezeRecovery(t *testing.T) {
	t.Helper()
	b := squeezeRecoveryBackoff
	squeezeRecoveryBackoff = 0
	t.Cleanup(func() { squeezeRecoveryBackoff = b })
}

func squeezeRecover(t *testing.T, pg *FakeTarget) error {
	t.Helper()
	ts := NewStaticTargetSet([]Target{pg}, 1)
	rec, err := DefaultRegistry.Apply(context.Background(),
		"cgroup_squeeze(target=pg, max_bytes=33554432)", ts)
	if err != nil {
		t.Fatal(err)
	}
	return rec(context.Background())
}

// The campaign soaks lost cells to a container restart racing the
// recovery exec (exit 137, "is restarting"). A transient failure must
// be retried — Start, re-lift the limit, restart PG — not recorded as
// a failed recovery.
func TestCgroupSqueezeRecovery_RetriesTransientExecFailure(t *testing.T) {
	fastSqueezeRecovery(t)
	n := 0
	pg := &FakeTarget{NameStr: "pg-0", RoleStr: "pg",
		ExecFunc: func([]string) ([]byte, error) {
			n++
			if n <= 2 {
				return nil, fmt.Errorf("docker exec: exit status 137")
			}
			return nil, nil
		}}
	if err := squeezeRecover(t, pg); err != nil {
		t.Fatalf("recovery should succeed once the cell comes back, got %v", err)
	}
	if n != 3 {
		t.Errorf("want 3 restart attempts, got %d", n)
	}
	if got := pg.StartCalls(); got != 2 {
		t.Errorf("every retry must Start the container; got %d Start calls", got)
	}
	// apply, lift, then one re-lift per retry.
	want := []int64{33554432, -1, -1, -1}
	if got := pg.MemoryLimits(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("every retry must re-lift the limit; got %v want %v", got, want)
	}
}

// A cell that never comes back is a real wedge: recovery must still
// fail, after a bounded number of attempts, naming the last cause.
func TestCgroupSqueezeRecovery_PersistentFailureIsBounded(t *testing.T) {
	fastSqueezeRecovery(t)
	n := 0
	pg := &FakeTarget{NameStr: "pg-0", RoleStr: "pg",
		ExecFunc: func([]string) ([]byte, error) {
			n++
			return nil, fmt.Errorf("pg did not come up")
		}}
	err := squeezeRecover(t, pg)
	if err == nil {
		t.Fatal("a cell that never recovers must fail the recovery")
	}
	if n != squeezeRecoveryAttempts {
		t.Errorf("want %d attempts, got %d", squeezeRecoveryAttempts, n)
	}
	if !strings.Contains(err.Error(), "pg did not come up") {
		t.Errorf("error should carry the last cause, got %v", err)
	}
}

// A Start that keeps failing is also bounded and reported.
func TestCgroupSqueezeRecovery_StartFailureReported(t *testing.T) {
	fastSqueezeRecovery(t)
	pg := &FakeTarget{NameStr: "pg-0", RoleStr: "pg",
		StartErr: fmt.Errorf("no such container"),
		ExecFunc: func([]string) ([]byte, error) {
			return nil, fmt.Errorf("%w", ErrTargetNotRunning)
		}}
	err := squeezeRecover(t, pg)
	if err == nil || !strings.Contains(err.Error(), "start container: no such container") {
		t.Fatalf("want start failure reported, got %v", err)
	}
}
