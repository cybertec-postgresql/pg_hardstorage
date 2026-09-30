package agent

import (
	"strings"
	"testing"
	"time"
)

// TestBuildRecoveryFromArgs_ToLatest pins that the control-plane
// to_latest arg (the CLI's --to-latest) arms target-less recovery, so a
// dispatched DR restore replays the whole archive instead of booting
// with only the backup's own WAL.
func TestBuildRecoveryFromArgs_ToLatest(t *testing.T) {
	r, err := buildRecoveryFromArgs(map[string]any{"to_latest": true, "to_action": "promote"}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if r == nil || !r.Enable {
		t.Fatalf("to_latest: recovery = %+v, want enabled", r)
	}
	if r.TargetLSN != "" || r.TargetName != "" || !r.TargetTime.IsZero() {
		t.Errorf("to_latest must set no target; got %+v", r)
	}
	if r.Action != "promote" {
		t.Errorf("Action = %q, want promote", r.Action)
	}
}

func TestBuildRecoveryFromArgs_ToLatestConflicts(t *testing.T) {
	_, err := buildRecoveryFromArgs(map[string]any{"to_latest": true, "to_lsn": "0/3000028"}, time.Time{})
	if err == nil || !strings.Contains(err.Error(), "to_latest") {
		t.Fatalf("want a to_latest conflict error; got %v", err)
	}
}

// TestBuildRecoveryFromArgs_SkipGapCheck pins the --skip-gap-check
// forward: the override reaches Recovery.SkipGapCheck.
func TestBuildRecoveryFromArgs_SkipGapCheck(t *testing.T) {
	r, err := buildRecoveryFromArgs(map[string]any{"to_lsn": "0/3000028", "skip_gap_check": true}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if !r.SkipGapCheck {
		t.Error("skip_gap_check=true did not reach Recovery.SkipGapCheck")
	}
}

func TestBoolArg_RejectsNonBool(t *testing.T) {
	if _, err := boolArg(map[string]any{"allow_foreign_cluster": "yes"}, "allow_foreign_cluster"); err == nil {
		t.Fatal("a non-bool safety override must be an error, not a silent false")
	}
	if v, err := boolArg(map[string]any{}, "x"); err != nil || v {
		t.Fatalf("absent = (%v, %v), want (false, nil)", v, err)
	}
}
