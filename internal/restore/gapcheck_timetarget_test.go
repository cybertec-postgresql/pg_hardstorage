package restore

// gapcheck_timetarget_test.go — the time/name-target gap refusal must
// be bounded by the seed backup's stop, exactly like the unbounded one.
//
// The composition that forced this: gapstate records are per-deployment
// and eternal — a pre-stream gap recorded at deployment birth outlives
// the backups it described. The seed for a `--to <time>` restore was
// already resolved (stop_time <= target) before this preflight, so its
// replay covers [seed.stop, target]; a gap ending at or below that stop
// is history the restore never touches. The original blanket rule
// (refuse on ANY recorded gap) predated having the stop threaded here,
// and after retention expired the init-era backup it turned into: every
// time-target restore of this deployment refuses FOREVER, over a window
// no surviving backup can even reach. A permanent false refusal is how
// operators learn to reach for --skip-gap-check reflexively — which
// disables the true refusals too.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/wal/gapstate"
)

func timeTargetRecovery() *Recovery {
	return &Recovery{Enable: true, Timeline: "latest", TargetTime: time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)}
}

// TestPreflightTimeTargetGap_GapBelowSeedStop_Allowed is bug #18's
// regression test: the recorded gap ends at 0/5000000, the chosen seed
// stops at 0/9000000 — replay starts above the gap and can never fall
// into it. Refusing here is the retention composition failure: after
// the init-era backup ages out, this is EVERY remaining backup.
func TestPreflightTimeTargetGap_GapBelowSeedStop_Allowed(t *testing.T) {
	sp := gapTestRepo(t)
	if _, err := gapstate.New(sp).Put(context.Background(), gapstate.Record{
		Deployment: gapTestDeployment, SlotName: "s", Timeline: 1,
		GapStartLSN: "0/3000000", GapEndLSN: "0/5000000", GapBytes: 0x2000000,
	}); err != nil {
		t.Fatal(err)
	}
	err := preflightWALGap(context.Background(), sp, gapTestDeployment,
		"0/9000000" /* seed stop ABOVE the whole gap */, timeTargetRecovery(), nil, nil)
	if err != nil {
		t.Fatalf("time-target PITR refused on a gap its seed can never reach: %v\n\n"+
			"Gap records outlive the backups they described. Once retention expires the "+
			"pre-gap generation, an unbounded refusal here means --to <time> never works "+
			"again for this deployment — a permanent false positive that trains operators "+
			"to pass --skip-gap-check, which then also bypasses the refusals that are true.", err)
	}
}

// TestPreflightTimeTargetGap_ReachableGap_StillRefuses: the seed stops
// BELOW the gap's end, so replay toward the target crosses the hole —
// the original refusal must survive the precision fix unchanged.
func TestPreflightTimeTargetGap_ReachableGap_StillRefuses(t *testing.T) {
	sp := gapTestRepo(t)
	if _, err := gapstate.New(sp).Put(context.Background(), gapstate.Record{
		Deployment: gapTestDeployment, SlotName: "s", Timeline: 1,
		GapStartLSN: "0/3000000", GapEndLSN: "0/5000000", GapBytes: 0x2000000,
	}); err != nil {
		t.Fatal(err)
	}
	err := preflightWALGap(context.Background(), sp, gapTestDeployment,
		"0/2000000" /* seed stop below the gap: replay crosses it */, timeTargetRecovery(), nil, nil)
	if err == nil {
		t.Fatal("time-target recovery over a REACHABLE gap was allowed — the precision " +
			"fix must narrow the refusal, not remove it")
	}
	if !strings.Contains(err.Error(), "target_in_wal_gap") {
		t.Errorf("wrong code: %v", err)
	}
	if oe, ok := output.AsOutputError(err); !ok || oe.Suggestion == nil ||
		!strings.Contains(oe.Suggestion.Human, "--skip-gap-check") {
		t.Errorf("refusal's suggestion does not name the override: %v", err)
	}
}

// TestPreflightTimeTargetGap_EmptyStop_Conservative: callers that
// cannot supply the seed's stop (older manifests predating StopLSN,
// defensive paths) keep the blanket posture — refusing wrongly is
// recoverable, allowing wrongly is not.
func TestPreflightTimeTargetGap_EmptyStop_Conservative(t *testing.T) {
	sp := gapTestRepo(t)
	if _, err := gapstate.New(sp).Put(context.Background(), gapstate.Record{
		Deployment: gapTestDeployment, SlotName: "s", Timeline: 1,
		GapStartLSN: "0/3000000", GapEndLSN: "0/5000000", GapBytes: 0x2000000,
	}); err != nil {
		t.Fatal(err)
	}
	if err := preflightWALGap(context.Background(), sp, gapTestDeployment,
		"" /* stop unknown */, timeTargetRecovery(), nil, nil); err == nil {
		t.Fatal("with no stop LSN to bound by, the time-target check must keep the " +
			"conservative blanket refusal, not assume unreachability")
	}
}

// TestPreflightTimeTargetGap_ManifestGapBelowStop_Allowed: the
// manifest-embedded gap record filters by the same reachability rule as
// the live gapstate one.
func TestPreflightTimeTargetGap_ManifestGapBelowStop_Allowed(t *testing.T) {
	sp := gapTestRepo(t)
	gaps := []backup.WALGap{{GapStartLSN: "0/1000000", GapEndLSN: "0/2000000"}}
	if err := preflightWALGap(context.Background(), sp, gapTestDeployment,
		"0/30001A0", timeTargetRecovery(), gaps, nil); err != nil {
		t.Fatalf("manifest gap below the seed's stop refused a time-target restore: %v", err)
	}
	// And the reachable direction still gates.
	gaps = []backup.WALGap{{GapStartLSN: "0/4000000", GapEndLSN: "0/6000000"}}
	if err := preflightWALGap(context.Background(), sp, gapTestDeployment,
		"0/30001A0", timeTargetRecovery(), gaps, nil); err == nil {
		t.Fatal("manifest gap ABOVE the seed's stop did not refuse")
	}
}

// TestEmitTimeTargetGapWarning_BoundedBySeedStop: the advisory event
// must apply the same reachability bound as the refusal. Warning about
// gaps this restore's replay can never touch trains the operator to
// ignore the warning that matters.
func TestEmitTimeTargetGapWarning_BoundedBySeedStop(t *testing.T) {
	sp := gapTestRepo(t)
	if _, err := gapstate.New(sp).Put(context.Background(), gapstate.Record{
		Deployment: gapTestDeployment, SlotName: "s", Timeline: 1,
		GapStartLSN: "0/3000000", GapEndLSN: "0/5000000", GapBytes: 0x2000000,
	}); err != nil {
		t.Fatal(err)
	}
	rec := timeTargetRecovery()

	var events []*output.Event
	emit := func(ev *output.Event) { events = append(events, ev) }

	// Unreachable: no advisory.
	emitTimeTargetGapWarning(context.Background(), sp, gapTestDeployment, "0/9000000", rec, nil, emit)
	if len(events) != 0 {
		t.Errorf("advisory fired for a gap the seed's replay can never reach: %+v", events[0])
	}
	// Reachable: advisory still fires.
	emitTimeTargetGapWarning(context.Background(), sp, gapTestDeployment, "0/2000000", rec, nil, emit)
	if len(events) == 0 {
		t.Error("no advisory for a REACHABLE gap — the bound must narrow the warning, " +
			"not silence it")
	}
	// Unknown stop: conservative, advisory fires.
	events = nil
	emitTimeTargetGapWarning(context.Background(), sp, gapTestDeployment, "", rec, nil, emit)
	if len(events) == 0 {
		t.Error("no advisory with an unknown stop LSN — unknown must mean conservative")
	}
}
