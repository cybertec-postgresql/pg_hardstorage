// plan_recovery_render_test.go — unit tests for planRecoveryFromRecovery,
// the issue-#99 echo of a Recovery target into the --preview body.
package restore

import (
	"testing"
	"time"
)

func TestPlanRecoveryFromRecovery_Nil(t *testing.T) {
	if got := planRecoveryFromRecovery(nil); got != nil {
		t.Errorf("nil Recovery should map to nil PlanRecovery; got %+v", got)
	}
}

func TestPlanRecoveryFromRecovery_LSNTarget(t *testing.T) {
	r := &Recovery{
		TargetLSN: "0/3000028",
		Inclusive: true,
		Action:    "promote",
		Timeline:  "latest",
	}
	got := planRecoveryFromRecovery(r)
	if got == nil {
		t.Fatal("nil PlanRecovery for an LSN target")
	}
	if got.TargetLSN != "0/3000028" {
		t.Errorf("TargetLSN = %q", got.TargetLSN)
	}
	if got.TargetTime != "" {
		t.Errorf("TargetTime should be empty for an LSN target; got %q", got.TargetTime)
	}
	if got.TargetName != "" {
		t.Errorf("TargetName should be empty; got %q", got.TargetName)
	}
	if !got.Inclusive || got.Action != "promote" || got.Timeline != "latest" {
		t.Errorf("side fields not mapped: %+v", got)
	}
}

// The time branch must render in UTC, RFC3339Nano. A non-UTC input
// must be converted, not emitted with its original offset — operators
// copying the value into postgresql.auto.conf expect an unambiguous
// UTC instant.
func TestPlanRecoveryFromRecovery_TimeTarget_RendersUTC(t *testing.T) {
	// 2026-04-27 09:42:00.5 at +05:30 == 04:12:00.5 UTC.
	loc := time.FixedZone("plus0530", 5*3600+30*60)
	r := &Recovery{
		TargetTime: time.Date(2026, 4, 27, 9, 42, 0, 500_000_000, loc),
		Inclusive:  false,
	}
	got := planRecoveryFromRecovery(r)
	if got == nil {
		t.Fatal("nil PlanRecovery for a time target")
	}
	want := "2026-04-27T04:12:00.5Z"
	if got.TargetTime != want {
		t.Errorf("TargetTime = %q; want %q (UTC RFC3339Nano)", got.TargetTime, want)
	}
	if got.TargetLSN != "" || got.TargetName != "" {
		t.Errorf("only TargetTime should be set; got %+v", got)
	}
	if got.Inclusive {
		t.Errorf("Inclusive should be false")
	}
}

func TestPlanRecoveryFromRecovery_NameTarget(t *testing.T) {
	r := &Recovery{
		TargetName: "before-the-drop",
		Inclusive:  true,
		Action:     "pause",
		Timeline:   "3",
	}
	got := planRecoveryFromRecovery(r)
	if got == nil {
		t.Fatal("nil PlanRecovery for a name target")
	}
	if got.TargetName != "before-the-drop" {
		t.Errorf("TargetName = %q", got.TargetName)
	}
	if got.TargetLSN != "" || got.TargetTime != "" {
		t.Errorf("only TargetName should be set; got %+v", got)
	}
	if got.Timeline != "3" {
		t.Errorf("Timeline = %q; want 3", got.Timeline)
	}
}
