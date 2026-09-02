package restore

// gapcheck_degraded_test.go — when the gap gate cannot read the live
// gap state it must say so.
//
// preflightWALGap has three consumers of gapstate: the LSN-target path,
// the time/name-target refusal, and an advisory warning. All three fall
// back to manifest-embedded gaps when gapstate.List fails, and that
// fallback is the right call — a transient backend error must not block
// a legitimate restore.
//
// But two of the three did it with a bare `_`, so the operator was
// never told the pre-flight had run against a partial picture. The
// unbounded path was the worst of them: it has no comment at all, and
// it is the one this file's own header singles out as suffering most
// from a hole —
//
//	"a STANDBY has no target by construction ... and a standby is the
//	 consumer that suffers most from a hole: it replays up to the
//	 missing segment and then waits."
//
// Its refusal is what "turns the silent-truncation failure into a typed
// error". Degrading it silently puts the operator back in exactly the
// failure the gate exists to prevent, while believing the gate ran.
//
// The LSN-target path already had this right: warn via
// gap_state_unreadable, then proceed. These tests pin that all the
// REFUSING paths behave the same way.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/faultinject"
)

var errGapStateDown = errors.New("simulated gapstate outage")

// gapStateUnreadable wraps sp so listing wal/<dep>/gaps/ fails while
// everything else works.
func gapStateUnreadable(t *testing.T, sp storage.StoragePlugin) storage.StoragePlugin {
	t.Helper()
	fi := faultinject.New(sp)
	fi.Activate([]faultinject.Rule{{
		Name:      "gapstate-list-down",
		Ops:       faultinject.OpList,
		KeyPrefix: "wal/" + gapTestDeployment + "/gaps/",
		Err:       errGapStateDown,
	}}, faultinject.ActivateOptions{})
	t.Cleanup(fi.Deactivate)
	return fi
}

// collectEvents returns an emit func plus a pointer to what it saw.
func collectEvents() (func(*output.Event), *[]*output.Event) {
	var seen []*output.Event
	return func(ev *output.Event) { seen = append(seen, ev) }, &seen
}

func hasEvent(events []*output.Event, action string) bool {
	for _, ev := range events {
		if ev != nil && ev.Op == action {
			return true
		}
	}
	return false
}

func TestPreflightWALGap_UnboundedReportsUnreadableGapState(t *testing.T) {
	sp := gapStateUnreadable(t, gapTestRepo(t))
	emit, seen := collectEvents()

	// Unbounded recovery: no target, so this is the standby /
	// --to-latest shape. The live gap state cannot be read.
	err := preflightWALGap(context.Background(), sp, gapTestDeployment,
		"0/30001A0", unboundedRecovery(), nil, emit)

	// Proceeding is correct — a transient backend error must not block
	// a legitimate restore.
	if err != nil {
		t.Fatalf("an unreadable gap state blocked the restore: %v\n\n"+
			"Blocking on a transient backend error trades a possible truncation for a "+
			"certain outage.", err)
	}
	if !hasEvent(*seen, "gap_state_unreadable") {
		t.Fatalf("the gap pre-flight degraded to manifest-only gaps and said nothing.\n\n"+
			"This is the unbounded path — standby and --to-latest — whose refusal is what "+
			"turns a silently truncated recovery into a typed error. The operator believes "+
			"the gate ran. Events seen: %v", actionsOf(*seen))
	}
}

func TestPreflightWALGap_TimeTargetReportsUnreadableGapState(t *testing.T) {
	sp := gapStateUnreadable(t, gapTestRepo(t))
	emit, seen := collectEvents()

	rec := &Recovery{Enable: true, Timeline: "latest",
		TargetTime: time.Date(2026, 4, 28, 15, 0, 0, 0, time.UTC)}
	if err := preflightWALGap(context.Background(), sp, gapTestDeployment,
		"0/30001A0", rec, nil, emit); err != nil {
		t.Fatalf("an unreadable gap state blocked a time-target restore: %v", err)
	}
	if !hasEvent(*seen, "gap_state_unreadable") {
		t.Fatalf("the time-target refusal ran against manifest-only gaps and said nothing; "+
			"events seen: %v", actionsOf(*seen))
	}
}

// A healthy gap state must not produce the warning, or it fires on
// every restore and stops carrying information.
func TestPreflightWALGap_HealthyGapStateEmitsNoDegradationWarning(t *testing.T) {
	sp := gapTestRepo(t)
	emit, seen := collectEvents()

	if err := preflightWALGap(context.Background(), sp, gapTestDeployment,
		"0/30001A0", unboundedRecovery(), nil, emit); err != nil {
		t.Fatalf("healthy repo: %v", err)
	}
	if hasEvent(*seen, "gap_state_unreadable") {
		t.Errorf("gap_state_unreadable fired on a healthy gap state; events: %v",
			actionsOf(*seen))
	}
}

func actionsOf(events []*output.Event) []string {
	out := make([]string, 0, len(events))
	for _, ev := range events {
		if ev != nil {
			out = append(out, ev.Op)
		}
	}
	if len(out) == 0 {
		return []string{"(none)"}
	}
	return out
}
