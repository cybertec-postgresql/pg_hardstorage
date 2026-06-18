// plan_reachability_test.go — unit tests for CheckTargetReachable (issue #99).
//
// CheckTargetReachable enforces "the operator's PITR target LSN
// must be at or after the backup's stop_lsn so forward WAL replay
// can reach it."  Without this check, PG silently replays to end
// of WAL when given an unreachable target — the operator sees a
// successful restore at the wrong point in time.  These tests
// pin every branch of the gate.
package restore

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

func TestCheckTargetReachable_NilRecovery(t *testing.T) {
	if err := CheckTargetReachable("0/3000028", nil); err != nil {
		t.Errorf("nil recovery should pass: %v", err)
	}
}

func TestCheckTargetReachable_NoTargetSet(t *testing.T) {
	// Recovery enabled but no target → end-of-WAL recovery.  Not
	// reachability-gated.
	r := &Recovery{Enable: true, RestoreCommand: "x"}
	if err := CheckTargetReachable("0/3000028", r); err != nil {
		t.Errorf("no target should pass: %v", err)
	}
}

func TestCheckTargetReachable_LSN_AfterStop(t *testing.T) {
	r := &Recovery{TargetLSN: "0/4000000", Inclusive: true}
	if err := CheckTargetReachable("0/3000028", r); err != nil {
		t.Errorf("LSN after stop should pass: %v", err)
	}
}

func TestCheckTargetReachable_LSN_EqualsStop_Inclusive(t *testing.T) {
	// Inclusive (PG default): recovery stops AT the target, so
	// target == stop_lsn is reachable.
	r := &Recovery{TargetLSN: "0/3000028", Inclusive: true}
	if err := CheckTargetReachable("0/3000028", r); err != nil {
		t.Errorf("inclusive LSN == stop should pass: %v", err)
	}
}

// The exclusive-stop boundary case: `--to-exclusive` makes PG
// stop JUST BEFORE the target, so target == stop_lsn means
// "stop one LSN earlier than the backup's checkpoint" —
// unreachable.  Pre-fix this slipped through; post-fix the gate
// must refuse so an operator who set --to-exclusive at exactly
// the backup's stop_lsn doesn't get a silent end-of-WAL recovery.
func TestCheckTargetReachable_LSN_EqualsStop_Exclusive_Refuses(t *testing.T) {
	r := &Recovery{TargetLSN: "0/3000028", Inclusive: false}
	err := CheckTargetReachable("0/3000028", r)
	if err == nil {
		t.Fatal("exclusive LSN == stop must refuse (target lands before checkpoint)")
	}
	var ce *output.Error
	if !errors.As(err, &ce) {
		t.Fatalf("err must be a structured *output.Error: %T %v", err, err)
	}
	if ce.Code != "restore.target_unreachable" {
		t.Errorf("code = %q; want restore.target_unreachable", ce.Code)
	}
	if !strings.Contains(ce.Error(), "exclusive") &&
		!strings.Contains(ce.Error(), "inclusive=false") {
		t.Errorf("error must explain the exclusive-stop relationship: %v", ce)
	}
}

// Exclusive AFTER stop must still pass — only the equality
// boundary differs between inclusive and exclusive modes.
func TestCheckTargetReachable_LSN_AfterStop_Exclusive(t *testing.T) {
	r := &Recovery{TargetLSN: "0/4000000", Inclusive: false}
	if err := CheckTargetReachable("0/3000028", r); err != nil {
		t.Errorf("exclusive LSN strictly after stop should pass: %v", err)
	}
}

// The canonical issue #99 case: target is BEFORE the backup's
// stop_lsn.  Pre-fix this slipped through; post-fix it must
// refuse with restore.target_unreachable and name both LSNs.
func TestCheckTargetReachable_LSN_BeforeStop_Refuses(t *testing.T) {
	r := &Recovery{TargetLSN: "0/3000000"}
	err := CheckTargetReachable("0/30001A0", r)
	if err == nil {
		t.Fatal("LSN before stop must refuse")
	}
	var ce *output.Error
	if !errors.As(err, &ce) {
		t.Fatalf("err must be a CodedError: %T %v", err, err)
	}
	if ce.Code != "restore.target_unreachable" {
		t.Errorf("code = %q; want restore.target_unreachable", ce.Code)
	}
	if !strings.Contains(ce.Error(), "0/3000000") || !strings.Contains(ce.Error(), "0/30001A0") {
		t.Errorf("error must name both LSNs: %v", ce)
	}
	if ce.Suggestion == nil {
		t.Errorf("error must carry a Suggestion")
	}
}

// A higher LSN second segment with the same first segment must be
// recognised as ABOVE the lower second-segment value — guards
// against a naive string comparison ("0/9" > "0/3000000" in
// lexicographic terms is false, but numerically true).  pglogrepl's
// uint64 parser is the source of truth, but this test pins that
// CheckTargetReachable uses it correctly.
func TestCheckTargetReachable_LSN_LexicographicHazard(t *testing.T) {
	// stop "0/9" = 9; target "0/3000000" = 50331648 (much higher).
	// Lexicographically "0/3000000" < "0/9", so a naive string
	// compare would refuse this — but numerically the target is
	// reachable.
	r := &Recovery{TargetLSN: "0/3000000"}
	if err := CheckTargetReachable("0/9", r); err != nil {
		t.Errorf("lexicographic compare hazard regressed: %v", err)
	}
}

// Cross-segment comparison: stop in segment "1/...", target in
// segment "2/..." — must accept.  Inverse must refuse.
func TestCheckTargetReachable_LSN_CrossSegment(t *testing.T) {
	r := &Recovery{TargetLSN: "2/0"}
	if err := CheckTargetReachable("1/FFFFFFFF", r); err != nil {
		t.Errorf("segment 2 after segment 1 should pass: %v", err)
	}
	r = &Recovery{TargetLSN: "1/0"}
	if err := CheckTargetReachable("2/0", r); err == nil {
		t.Error("segment 1 before segment 2 should refuse")
	}
}

func TestCheckTargetReachable_TimeTarget_NotGated(t *testing.T) {
	// Time targets can't be statically range-checked (PG resolves
	// to an LSN at recovery time).  The function must skip them
	// — the wal-gap pre-flight handles known-bad time targets.
	r := &Recovery{TargetTime: time.Now().UTC()}
	if err := CheckTargetReachable("0/3000028", r); err != nil {
		t.Errorf("time target should not be reachability-gated: %v", err)
	}
}

func TestCheckTargetReachable_NameTarget_NotGated(t *testing.T) {
	r := &Recovery{TargetName: "before_drop"}
	if err := CheckTargetReachable("0/3000028", r); err != nil {
		t.Errorf("name target should not be reachability-gated: %v", err)
	}
}

func TestCheckTargetReachable_MalformedTarget(t *testing.T) {
	r := &Recovery{TargetLSN: "garbage"}
	err := CheckTargetReachable("0/3000028", r)
	if err == nil {
		t.Fatal("malformed target_lsn must refuse")
	}
	var ce *output.Error
	if !errors.As(err, &ce) {
		t.Fatalf("err must be a CodedError: %T %v", err, err)
	}
	if ce.Code != "usage.bad_target_lsn" {
		t.Errorf("code = %q; want usage.bad_target_lsn", ce.Code)
	}
}

func TestCheckTargetReachable_MalformedStopLSN(t *testing.T) {
	// A malformed StopLSN is a manifest-level corruption.  Surface
	// as manifest.invalid so the operator's mental model matches —
	// this is a backup-side problem, not a flag-side problem.
	r := &Recovery{TargetLSN: "0/3000028"}
	err := CheckTargetReachable("not-an-lsn", r)
	if err == nil {
		t.Fatal("malformed stop_lsn must refuse")
	}
	var ce *output.Error
	if !errors.As(err, &ce) {
		t.Fatalf("err must be a CodedError: %T %v", err, err)
	}
	if ce.Code != "manifest.invalid" {
		t.Errorf("code = %q; want manifest.invalid", ce.Code)
	}
}
