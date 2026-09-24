package inject_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/inject"
)

const mib = 1 << 20

// dfTarget answers `df` with before on the first call and after on
// every later one, and records everything else.
func dfTarget(before, after int64) *inject.FakeTarget {
	var n atomic.Int32
	return &inject.FakeTarget{NameStr: "pg-0", RoleStr: "pg",
		ExecFunc: func(argv []string) ([]byte, error) {
			if argv[0] == "df" {
				v := after
				if n.Add(1) == 1 {
					v = before
				}
				return []byte("Avail\n" + itoa(v) + "\n"), nil
			}
			return nil, nil
		}}
}

func itoa(n int64) string {
	b := []byte{}
	if n == 0 {
		return "0"
	}
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func ran(tg *inject.FakeTarget, sub string) bool {
	for _, c := range tg.ExecCalls() {
		if strings.Contains(strings.Join(c, " "), sub) {
			return true
		}
	}
	return false
}

// disk_full capped its spacer at 256 MiB by default, so on any real
// host it consumed a sliver of free space, produced no ENOSPC — and was
// reported as applied. A fill the cap cannot reach is not applied, and
// must say so rather than claim a fault that never happened.
func TestDiskFull_UnreachableFillIsNotApplied(t *testing.T) {
	pg := dfTarget(500*1024*mib, 500*1024*mib) // 500 GiB free
	ts := inject.NewStaticTargetSet([]inject.Target{pg}, 1)
	_, err := inject.DefaultRegistry.Apply(context.Background(), "disk_full(target=pg, fill=98%)", ts)
	if !errors.Is(err, inject.ErrNotApplicable) {
		t.Fatalf("a 256 MiB spacer on a 500 GiB filesystem fills nothing; want ErrNotApplicable, got %v", err)
	}
	if ran(pg, "dd if=/dev/zero") {
		t.Error("a fill that cannot reach its target must not write a spacer at all")
	}
}

// Where the fill IS reachable it is applied — and checked: the free
// space afterwards must really be gone.
func TestDiskFull_ReachableFillIsAppliedAndChecked(t *testing.T) {
	pg := dfTarget(100*mib, 1*mib)
	ts := inject.NewStaticTargetSet([]inject.Target{pg}, 1)
	rec, err := inject.DefaultRegistry.Apply(context.Background(), "disk_full(target=pg, fill=98%)", ts)
	if err != nil {
		t.Fatal(err)
	}
	if !ran(pg, "count=98") {
		t.Errorf("98%% of 100 MiB is 98 blocks; calls: %v", pg.ExecCalls())
	}
	if err := rec(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestDiskFull_FillThatDidNotLandFails(t *testing.T) {
	pg := dfTarget(100*mib, 100*mib) // dd "ran" but nothing was consumed
	ts := inject.NewStaticTargetSet([]inject.Target{pg}, 1)
	_, err := inject.DefaultRegistry.Apply(context.Background(), "disk_full(target=pg, fill=98%)", ts)
	if err == nil {
		t.Fatal("the free space did not drop; the fault must not report itself applied")
	}
	if !ran(pg, "rm -f") {
		t.Error("a failed fill must remove its partial spacer")
	}
}

// pause_archive touched a sentinel nothing reads: archiving never
// paused. It now stops the WAL archiving processes themselves.
func TestPauseArchive_StopsTheArchiver(t *testing.T) {
	agent := &inject.FakeTarget{NameStr: "agent-0", RoleStr: "agent",
		ExecFunc: func(argv []string) ([]byte, error) {
			s := strings.Join(argv, " ")
			if strings.Contains(s, "-STOP") {
				return []byte("stopped: 41 97\n"), nil
			}
			return []byte("resumed\n"), nil
		}}
	ts := inject.NewStaticTargetSet([]inject.Target{agent}, 1)
	rec, err := inject.DefaultRegistry.Apply(context.Background(), "pause_archive(target=agent)", ts)
	if err != nil {
		t.Fatal(err)
	}
	if ran(agent, "touch") {
		t.Error("pause_archive must not rely on a sentinel file nothing reads")
	}
	if err := rec(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !ran(agent, "-CONT") || !ran(agent, "41 97") {
		t.Errorf("recovery must SIGCONT the stopped pids; calls: %v", agent.ExecCalls())
	}
}

func TestPauseArchive_NoArchiverIsNotApplied(t *testing.T) {
	agent := &inject.FakeTarget{NameStr: "agent-0", RoleStr: "agent", ExecDefault: []byte("stopped:\n")}
	ts := inject.NewStaticTargetSet([]inject.Target{agent}, 1)
	_, err := inject.DefaultRegistry.Apply(context.Background(), "pause_archive(target=agent)", ts)
	if !errors.Is(err, inject.ErrNotApplicable) {
		t.Fatalf("nothing to pause; want ErrNotApplicable, got %v", err)
	}
}

func TestPauseArchive_RecoveryReportsAProcessStillStopped(t *testing.T) {
	agent := &inject.FakeTarget{NameStr: "agent-0", RoleStr: "agent",
		ExecFunc: func(argv []string) ([]byte, error) {
			if strings.Contains(strings.Join(argv, " "), "-STOP") {
				return []byte("stopped: 41\n"), nil
			}
			return []byte("still-stopped: 41\n"), nil
		}}
	ts := inject.NewStaticTargetSet([]inject.Target{agent}, 1)
	rec, err := inject.DefaultRegistry.Apply(context.Background(), "pause_archive(target=agent)", ts)
	if err != nil {
		t.Fatal(err)
	}
	if err := rec(context.Background()); err == nil {
		t.Fatal("an archiver left stopped must fail the recovery")
	}
}

// Recoveries that discarded their Exec errors returned nil — the
// fault stayed applied and was reported recovered.
func TestRecoveries_ReportRevertFailures(t *testing.T) {
	for _, tc := range []struct{ action, role, failOn string }{
		{"network_block(target=10.0.0.1)", "agent", "-D"},
		{"libfaketime(skew=+2d)", "agent", "faketimerc"},
		{"toxiproxy(proxy=repo, type=latency, latency=100)", "toxiproxy", "remove"},
	} {
		t.Run(tc.action, func(t *testing.T) {
			tg := &inject.FakeTarget{NameStr: "t", RoleStr: tc.role,
				ExecFunc: func(argv []string) ([]byte, error) {
					if strings.Contains(strings.Join(argv, " "), tc.failOn) {
						return nil, errors.New("revert refused")
					}
					return nil, nil
				}}
			ts := inject.NewStaticTargetSet([]inject.Target{tg}, 1)
			rec, err := inject.DefaultRegistry.Apply(context.Background(), tc.action, ts)
			if err != nil {
				t.Fatal(err)
			}
			if err := rec(context.Background()); err == nil {
				t.Fatal("the revert failed; recovery must say so")
			}
		})
	}
}
