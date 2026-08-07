package gameday_test

// patroni_failover_test.go — the drill must be able to fail.
//
// This scenario previously appended a "deferred" note and returned
// Pass=true unconditionally: `gameday run patroni_failover`, declared
// tier L4, exited 0 having promoted nothing and measured nothing. Every
// case below is a way the drill can now come back false, which is the
// property that makes the green ones mean something.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/gameday"
)

// fakePatroni is a scriptable PatroniDriver.
type fakePatroni struct {
	leaders    []string // returned in order; last value repeats
	calls      int
	switchErr  error
	switchSeen string
	leaderErr  error
}

func (f *fakePatroni) Leader(context.Context) (string, uint32, error) {
	if f.leaderErr != nil {
		return "", 0, f.leaderErr
	}
	i := f.calls
	f.calls++
	if i >= len(f.leaders) {
		i = len(f.leaders) - 1
	}
	return f.leaders[i], uint32(7 + i), nil
}

func (f *fakePatroni) Switchover(_ context.Context, leader string) error {
	f.switchSeen = leader
	return f.switchErr
}

func run(t *testing.T, opts gameday.RunOptions) *gameday.Result {
	t.Helper()
	res, err := gameday.Run(context.Background(), "patroni_failover", opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

func TestPatroniFailover_RefusesWithoutACluster(t *testing.T) {
	res := run(t, gameday.RunOptions{})
	if res.Pass {
		t.Fatal("scenario passed with no Patroni endpoint configured — it promoted nothing " +
			"and measured nothing, which is precisely the false green this replaced")
	}
	if !strings.Contains(res.Failure, "patroni.url") {
		t.Errorf("failure text should tell the operator what to configure, got %q", res.Failure)
	}
	if res.Deferred {
		t.Error("a missing endpoint is a configuration problem, not an unimplemented " +
			"scenario; Deferred would send the operator looking for the wrong thing")
	}
}

func TestPatroniFailover_PassesWhenLeaderMovesAndSlotSurvives(t *testing.T) {
	drv := &fakePatroni{leaders: []string{"node-1", "node-2"}}
	res := run(t, gameday.RunOptions{
		Patroni: drv,
		ObserveSlot: func(context.Context) (*gameday.SlotObservation, error) {
			return &gameday.SlotObservation{Outcome: "found", GapBytes: 0}, nil
		},
		RecoverWithin: 10 * time.Second,
	})
	if !res.Pass {
		t.Fatalf("drill failed on a clean switchover: %s", res.Failure)
	}
	if drv.switchSeen != "node-1" {
		t.Errorf("switchover named %q as the leader to demote, want node-1 — Patroni "+
			"rejects a mismatched leader, which is the guard against racing a failover "+
			"that already happened", drv.switchSeen)
	}
	if res.RecoveryTime == 0 {
		t.Error("RecoveryTime not recorded; the drill cannot report how long promotion took")
	}
}

func TestPatroniFailover_FailsWhenSlotGapOpens(t *testing.T) {
	calls := 0
	res := run(t, gameday.RunOptions{
		Patroni: &fakePatroni{leaders: []string{"node-1", "node-2"}},
		ObserveSlot: func(context.Context) (*gameday.SlotObservation, error) {
			calls++
			if calls == 1 {
				return &gameday.SlotObservation{Outcome: "found"}, nil
			}
			return &gameday.SlotObservation{Outcome: "recreated", GapBytes: 4096}, nil
		},
		RecoverWithin: 10 * time.Second,
	})
	if res.Pass {
		t.Fatal("drill passed while the slot was recreated 4 KiB past our last confirmed " +
			"LSN — that WAL cannot be fetched again and PITR inside the window is impossible")
	}
	if !strings.Contains(res.Failure, "gap_bytes=4096") {
		t.Errorf("failure should quantify the gap, got %q", res.Failure)
	}
}

func TestPatroniFailover_FailsWhenPatroniRefuses(t *testing.T) {
	res := run(t, gameday.RunOptions{
		Patroni: &fakePatroni{
			leaders:   []string{"node-1"},
			switchErr: errors.New("switchover refused by Patroni: no candidate"),
		},
		RecoverWithin: 5 * time.Second,
	})
	if res.Pass {
		t.Fatal("drill passed when Patroni declined to switch over — a cluster with no " +
			"healthy replica to promote is exactly the finding a drill exists to surface")
	}
}

func TestPatroniFailover_FailsWhenLeaderNeverMoves(t *testing.T) {
	res := run(t, gameday.RunOptions{
		Patroni:       &fakePatroni{leaders: []string{"node-1"}}, // never changes
		RecoverWithin: 2 * time.Second,
	})
	if res.Pass {
		t.Fatal("drill passed while the original leader still held the lock — the " +
			"switchover was accepted but never took effect, which is a silent failure " +
			"in production")
	}
	if !strings.Contains(res.Failure, "still holds the leader") {
		t.Errorf("failure should say the old leader never moved, got %q", res.Failure)
	}
}

// TestPatroniFailover_UnmeasuredIsDeferredNotAPass: without the
// ObserveSlot seam the drill has driven a real switchover and watched
// the leader move — but it has NOT checked the invariant it exists for,
// which is that the promotion did not cost us WAL.
//
// This test previously required the opposite: Pass=true with evidence
// tagged "unmeasured". That was wrong twice over. It is a hollow pass
// at tier L4 — the tier an auditor reads as "we tested catastrophic
// failover" — and the word "unmeasured" is what let it through, because
// deferred_not_pass_test.go keys on the exact string "deferred". The
// guard written to catch evidence-of-no-drive alongside Pass=true was
// sitting right there and could not see a synonym.
//
// So: Deferred, Pass=false, with a Failure that names what to configure.
// The CLI maps Deferred to notimpl.scenario, so an operator reads "not
// implemented" rather than "invariant violated".
func TestPatroniFailover_UnmeasuredIsDeferredNotAPass(t *testing.T) {
	res := run(t, gameday.RunOptions{
		Patroni:       &fakePatroni{leaders: []string{"node-1", "node-2"}},
		RecoverWithin: 10 * time.Second,
	})
	if res.Pass {
		t.Fatal("the drill reported a pass without measuring slot continuity.\n\n" +
			"The leader moved, which is not evidence that the promotion kept the WAL. " +
			"`gameday run patroni_failover` would exit 0 and `gameday report` would count " +
			"a success, indistinguishable from a run that actually checked the invariant.")
	}
	if !res.Deferred {
		t.Error("Result.Deferred is not set; the CLI cannot tell an unwired seam apart " +
			"from a genuine invariant failure and would report verify.failed (exit 9)")
	}
	if res.Failure == "" {
		t.Error("deferred with no Failure text: the operator gets a non-zero exit with no " +
			"explanation of what to configure")
	}
	var labelled bool
	for _, ev := range res.Evidence {
		if ev.Kind == "deferred" {
			labelled = true
		}
	}
	if !labelled {
		t.Error("no evidence tagged `deferred`. That exact string is what " +
			"TestScenarios_DeferredIsNeverAPass keys on; a synonym here re-opens the hole " +
			"this test is closing.")
	}
}
