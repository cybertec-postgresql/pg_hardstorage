// recovery_render_test.go — gap-fill coverage for
// WriteRecoveryFiles render paths not pinned by recovery_test.go.
//
// Each test here pins a rendered GUC or on-disk file shape that an
// operator relies on at recovery time.  A regression in any of
// these is a real-world "PG silently does the wrong thing" bug,
// the same class issue #99 was.
package restore_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/restore"
)

// TestWriteRecoveryFiles_InclusiveFalse: --to-exclusive must
// render as `recovery_target_inclusive = false` in
// postgresql.auto.conf.  Pre-fix recovery_test.go covered the
// default-true case (TestWriteRecoveryFiles_LSN) but never
// asserted the false rendering — a render-time inversion would
// have shipped silently and made every exclusive-stop PITR
// behave like an inclusive one.
func TestWriteRecoveryFiles_InclusiveFalse(t *testing.T) {
	dir := t.TempDir()
	if err := restore.WriteRecoveryFiles(dir, restore.Recovery{
		Enable:         true,
		RestoreCommand: "x",
		TargetLSN:      "0/3000028",
		Inclusive:      false, // --to-exclusive on the CLI
	}); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "postgresql.auto.conf"))
	if !strings.Contains(string(body), "recovery_target_inclusive = false") {
		t.Errorf("expected recovery_target_inclusive = false; got:\n%s", body)
	}
	if strings.Contains(string(body), "recovery_target_inclusive = true") {
		t.Errorf("inclusive=false render leaked a true line; got:\n%s", body)
	}
}

// TestWriteRecoveryFiles_TimelineExplicitInt: an operator pinning
// a specific timeline (e.g. "3" to follow a known failover lineage
// instead of "latest") must see the integer quoted as a string in
// the GUC.  Pre-existing tests only covered "latest"; a future
// quoteSQL change for ints would have been undetected.
func TestWriteRecoveryFiles_TimelineExplicitInt(t *testing.T) {
	dir := t.TempDir()
	if err := restore.WriteRecoveryFiles(dir, restore.Recovery{
		Enable:         true,
		RestoreCommand: "x",
		TargetLSN:      "0/3000028",
		Inclusive:      true,
		Timeline:       "3",
	}); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "postgresql.auto.conf"))
	// PG accepts the value as a single-quoted string at the GUC
	// boundary; the underlying GUC parser converts back to an int.
	if !strings.Contains(string(body), "recovery_target_timeline = '3'") {
		t.Errorf("expected recovery_target_timeline = '3'; got:\n%s", body)
	}
}

// TestWriteRecoveryFiles_StandbyMode: StandbyMode=true drops
// standby.signal and NOT recovery.signal.  The two files have
// different semantics in PG: recovery.signal → recover to target
// then promote; standby.signal → recover forever, accepting WAL
// from restore_command until manually promoted.  A wrong file
// here is the difference between a PITR'd cluster and a hot
// standby — pretty meaningful for the operator.
func TestWriteRecoveryFiles_StandbyMode(t *testing.T) {
	dir := t.TempDir()
	if err := restore.WriteRecoveryFiles(dir, restore.Recovery{
		Enable:         true,
		RestoreCommand: "x",
		StandbyMode:    true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "standby.signal")); err != nil {
		t.Errorf("standby.signal missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "recovery.signal")); err == nil {
		t.Error("recovery.signal must NOT exist in standby mode (PG would treat as PITR)")
	}
}

// TestWriteRecoveryFiles_StandbyMode_RejectsTarget: pre-existing
// validateRecovery enforces the mutual exclusion; this test pins
// it from the user-facing WriteRecoveryFiles surface to guard
// against a refactor that bypasses validateRecovery's check.
func TestWriteRecoveryFiles_StandbyMode_RejectsTarget(t *testing.T) {
	dir := t.TempDir()
	err := restore.WriteRecoveryFiles(dir, restore.Recovery{
		Enable:         true,
		RestoreCommand: "x",
		StandbyMode:    true,
		TargetLSN:      "0/3000028",
	})
	if err == nil {
		t.Fatal("standby + target must refuse at the user-facing surface")
	}
	if !strings.Contains(err.Error(), "StandbyMode") {
		t.Errorf("error should name StandbyMode; got %v", err)
	}
	// No signal files should have been written.
	if _, err := os.Stat(filepath.Join(dir, "standby.signal")); err == nil {
		t.Error("standby.signal written despite refusal")
	}
	if _, err := os.Stat(filepath.Join(dir, "recovery.signal")); err == nil {
		t.Error("recovery.signal written despite refusal")
	}
}

// TestWriteRecoveryFiles_AllThreeTargetsAtOnce: validateRecovery
// covers pairs; this pins the triple-set case from the user-
// facing surface (same defence-in-depth posture as the standby
// test above).
func TestWriteRecoveryFiles_AllThreeTargetsAtOnce(t *testing.T) {
	dir := t.TempDir()
	err := restore.WriteRecoveryFiles(dir, restore.Recovery{
		Enable:         true,
		RestoreCommand: "x",
		TargetLSN:      "0/3000028",
		TargetName:     "n",
		Inclusive:      true,
	})
	if err == nil {
		t.Fatal("expected refusal on multi-target")
	}
	if !strings.Contains(err.Error(), "at most one") {
		t.Errorf("error should explain the at-most-one rule; got %v", err)
	}
}

// TestWriteRecoveryFiles_TimeTarget_ExplicitOffset: the time-
// target GUC must render with an explicit `+00` offset so PG's
// parser resolves it unambiguously to UTC.  A bare timestamp (no
// offset) is interpreted in PG's `timezone` GUC, which the
// operator may have set to anything.  This is the same class of
// bug issue #70 fixed for the CLI input side — pin the OUTPUT
// side so the same drift can't reappear.
func TestWriteRecoveryFiles_TimeTarget_ExplicitOffset(t *testing.T) {
	dir := t.TempDir()
	// Stamp a time deliberately in a non-UTC location so an
	// accidental "use whatever zone the time.Time carried" would
	// produce a different string.
	target, err := time.Parse(time.RFC3339, "2026-05-15T14:30:00-04:00")
	if err != nil {
		t.Fatal(err)
	}
	if err := restore.WriteRecoveryFiles(dir, restore.Recovery{
		Enable:         true,
		RestoreCommand: "x",
		TargetTime:     target,
		Inclusive:      true,
	}); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "postgresql.auto.conf"))
	// 14:30 -04:00 == 18:30 UTC.  Must be rendered in UTC with
	// the +00 suffix.
	if !strings.Contains(string(body), "recovery_target_time = '2026-05-15 18:30:00+00'") {
		t.Errorf("expected explicit-UTC render; got:\n%s", body)
	}
}

// TestWriteRecoveryFiles_ActionShutdown: covers the third arm of
// the action enum (pause / promote / shutdown).  Pre-existing
// tests covered pause + promote; shutdown is the on-failure-
// shutdown PITR pattern operators use during forensic restores
// (PG stops cleanly at the target instead of pausing or
// promoting).
func TestWriteRecoveryFiles_ActionShutdown(t *testing.T) {
	dir := t.TempDir()
	if err := restore.WriteRecoveryFiles(dir, restore.Recovery{
		Enable:         true,
		RestoreCommand: "x",
		TargetLSN:      "0/3000028",
		Inclusive:      true,
		Action:         "shutdown",
	}); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "postgresql.auto.conf"))
	if !strings.Contains(string(body), "recovery_target_action = 'shutdown'") {
		t.Errorf("expected shutdown action; got:\n%s", body)
	}
}

// TestWriteAutoRecovery_PrimesNonPITRRestore: WriteAutoRecovery
// is the no-target sibling of WriteRecoveryFiles — used by plain
// restores to drop standby.signal + recovery_target='immediate'
// so the restored cluster reaches consistency on first start.
// Without this primer the restored cluster sits in standby
// forever waiting for the missing post-checkpoint WAL segment
// (the soak testing failure cluster).  Pin the on-disk shape so
// the primer doesn't silently regress.
func TestWriteAutoRecovery_PrimesNonPITRRestore(t *testing.T) {
	dir := t.TempDir()
	if err := restore.WriteAutoRecovery(dir, "db1", "file:///r"); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "postgresql.auto.conf"))
	got := string(body)
	for _, want := range []string{
		"recovery_target = 'immediate'",
		"recovery_target_action = 'promote'",
		"restore_command",
		"wal fetch",
		"file:///r",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("auto-recovery primer missing %q; got:\n%s", want, got)
		}
	}
	// Auto-recovery is signalled via standby.signal (not
	// recovery.signal) so PG stays in recovery until it can
	// promote — same primer pattern as a real standby.
	if _, err := os.Stat(filepath.Join(dir, "standby.signal")); err != nil {
		t.Errorf("standby.signal missing from auto-recovery primer: %v", err)
	}
}

// TestWriteAutoRecovery_NeverSignalWithoutRestoreCommand pins the
// bug-#73 fix: when a repo URL and deployment ARE supplied, the primer
// must NOT drop standby.signal onto disk without a restore_command.
// Previously, if os.Executable() errored, the restore_command line was
// silently skipped while standby.signal was still written — producing a
// cluster that enters recovery and then waits forever for WAL it has no
// way to fetch.  The fix couples the two: on the happy path both are
// present; on the (unforceable-in-test) os.Executable failure path the
// call returns an error instead of writing a half-armed data dir.  Here
// we assert the coupling holds on the happy path — a standby.signal is
// never emitted alongside an auto-recovery block lacking a
// restore_command.
func TestWriteAutoRecovery_NeverSignalWithoutRestoreCommand(t *testing.T) {
	dir := t.TempDir()
	if err := restore.WriteAutoRecovery(dir, "db1", "file:///r"); err != nil {
		t.Fatalf("WriteAutoRecovery: %v", err)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "postgresql.auto.conf"))
	_, sigErr := os.Stat(filepath.Join(dir, "standby.signal"))
	if sigErr == nil && !strings.Contains(string(body), "restore_command") {
		t.Fatalf("standby.signal written WITHOUT a restore_command — bug #73 regressed; conf:\n%s", body)
	}

	// Signal-only mode (empty repo + deployment) is the documented
	// escape hatch for offline / synthesised-manifest use, and stays
	// legal: no restore_command, and that is intentional.
	dir2 := t.TempDir()
	if err := restore.WriteAutoRecovery(dir2, "", ""); err != nil {
		t.Fatalf("WriteAutoRecovery signal-only: %v", err)
	}
	body2, _ := os.ReadFile(filepath.Join(dir2, "postgresql.auto.conf"))
	if strings.Contains(string(body2), "restore_command") {
		t.Errorf("signal-only primer should not emit a restore_command; got:\n%s", body2)
	}
}
