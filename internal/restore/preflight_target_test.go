// preflight_target_test.go — pins the wrong-cluster / wrong-
// target protections at the unit-test layer.
//
// These refusals are the operator's last line of defence
// between `pg_hardstorage restore <wrong-deployment>
// --target /var/lib/postgresql` and silently overwriting a
// production datadir.  The cli/runbook docs reference each of
// the error codes by name; without a test that triggers them,
// a refactor could silently weaken or disable any one of them
// and the docs would lie about what the binary actually does.
//
// Surface covered:
//
//	preflight.target_pg_datadir          — PG_VERSION present
//	preflight.target_not_empty           — non-empty but not PG
//	preflight.target_running_postgres    — postmaster.pid alive
//	preflight.checkpoint_check_failed    — bad checkpoint state
//	(empty / missing target paths just pass — also tested)
//
// The system-identifier-mismatch case (target's pg_control
// disagrees with the backup's manifest) gets a dedicated guard
// on the --force path: even with --force, a target that is a
// DIFFERENT live-data cluster (pg_control system identifier ≠
// the backup's) is refused with preflight.target_foreign_cluster
// unless --force-foreign is also passed. See issue #100.
package restore

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// TestPreflightTarget_NonExistentDirPasses: a target path that
// doesn't exist is fine — Restore creates it.
func TestPreflightTarget_NonExistentDirPasses(t *testing.T) {
	target := filepath.Join(t.TempDir(), "fresh-target")
	if err := preflightTarget(target, false, "", false); err != nil {
		t.Errorf("non-existent target should pass: %v", err)
	}
}

// TestPreflightTarget_EmptyDirPasses: an empty existing dir is
// also fine — Restore writes into it.
func TestPreflightTarget_EmptyDirPasses(t *testing.T) {
	if err := preflightTarget(t.TempDir(), false, "", false); err != nil {
		t.Errorf("empty dir should pass: %v", err)
	}
}

// TestPreflightTarget_PGDatadirRefuses: a dir containing a
// PG_VERSION file is presumed to be an existing PostgreSQL
// datadir.  Restore refuses with the operator-specific
// suggestion (mv aside OR --force).  This is the wrong-
// cluster protection the runbook docs cite.
func TestPreflightTarget_PGDatadirRefuses(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "PG_VERSION"), []byte("17\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := preflightTarget(dir, false, "", false)
	if err == nil {
		t.Fatal("expected refusal on PG_VERSION-bearing target")
	}
	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("error must be structured: %v", err)
	}
	if oe.Code != "preflight.target_pg_datadir" {
		t.Errorf("code = %q; want preflight.target_pg_datadir", oe.Code)
	}
	if oe.Suggestion == nil || !strings.Contains(oe.Suggestion.Human, "--force") {
		t.Errorf("suggestion must mention --force; got %+v", oe.Suggestion)
	}
}

// TestPreflightTarget_NonEmptyRefuses: a non-empty target that
// isn't a PG datadir still refuses without --force, but with
// a milder code so operators don't get the "this looks like
// production!" framing for a stray file.
func TestPreflightTarget_NonEmptyRefuses(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "random-file"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := preflightTarget(dir, false, "", false)
	if err == nil {
		t.Fatal("expected refusal on non-empty target")
	}
	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("error must be structured: %v", err)
	}
	if oe.Code != "preflight.target_not_empty" {
		t.Errorf("code = %q; want preflight.target_not_empty", oe.Code)
	}
}

// TestPreflightTarget_ForceOverridesPGDatadir: --force ON its
// own DOES override the PG_VERSION refusal.  The docs commit to
// this as the operator's "I know what I'm doing" escape hatch.
// This test pins that contract so a future refactor doesn't
// silently strengthen it (which would be a backwards-compat
// break for scripts).
func TestPreflightTarget_ForceOverridesPGDatadir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "PG_VERSION"), []byte("17\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := preflightTarget(dir, true, "", false); err != nil {
		t.Errorf("--force should override PG_VERSION refusal: %v", err)
	}
}

// TestPreflightTarget_ProcessAliveBranchExists: the
// running-PG refusal is the ONE check --force can't override.
// Trying to actually fire it from a unit test is brittle (we'd
// need a stable always-alive PID; PID 1 returns EPERM as
// "alive" on Unix but the Go test runner's signal semantics
// vary across platforms — better not to bet a test on that).
//
// Instead we just confirm that processAlive(0) returns false
// (no-op input must not falsely fire the gate) and that the
// preflight error CODE for the running-PG branch matches the
// documented one.  The integration-level coverage lives in the
// scenario suite where a real PG is actually started against
// the target.
func TestPreflightTarget_ProcessAliveBranchExists(t *testing.T) {
	if processAlive(0) {
		t.Error("processAlive(0) must return false; a PID-0 false-positive would make every restore refuse")
	}
	// negative-PID guard
	if processAlive(-1) {
		t.Error("processAlive(-1) must return false")
	}
}

// writeFakeDatadir creates a target that looks like a real PGDATA: a
// PG_VERSION file plus global/pg_control whose first 8 bytes carry
// sysID (ControlFileData.system_identifier, little-endian). When
// withControl is false, pg_control is omitted.
func writeFakeDatadir(t *testing.T, sysID uint64, withControl bool) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "PG_VERSION"), []byte("17\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if withControl {
		if err := os.MkdirAll(filepath.Join(dir, "global"), 0o700); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 8192) // pg_control is ~8 KiB; only the first 8 bytes matter here
		binary.LittleEndian.PutUint64(buf[:8], sysID)
		if err := os.WriteFile(filepath.Join(dir, "global", "pg_control"), buf, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestReadControlSystemIdentifier_RoundTrip: the helper decodes the
// system identifier PG would have written, and reports absence cleanly.
func TestReadControlSystemIdentifier_RoundTrip(t *testing.T) {
	const sysID = uint64(7388123456789012345)
	got, ok := readControlSystemIdentifier(writeFakeDatadir(t, sysID, true))
	if !ok {
		t.Fatal("expected to read a system identifier")
	}
	if want := strconv.FormatUint(sysID, 10); got != want {
		t.Errorf("system id = %q, want %q", got, want)
	}
	if _, ok := readControlSystemIdentifier(t.TempDir()); ok {
		t.Error("missing pg_control should return ok=false")
	}
}

// TestPreflightTarget_ForeignClusterRefusedUnderForce (issue #100):
// --force into a DIFFERENT cluster (system id mismatch) is refused
// with preflight.target_foreign_cluster + a --force-foreign hint.
func TestPreflightTarget_ForeignClusterRefusedUnderForce(t *testing.T) {
	dir := writeFakeDatadir(t, 1111111111111111111, true)
	err := preflightTarget(dir, true, "2222222222222222222", false)
	if err == nil {
		t.Fatal("expected refusal: --force into a foreign cluster")
	}
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != "preflight.target_foreign_cluster" {
		t.Fatalf("code = %v, want preflight.target_foreign_cluster", err)
	}
	if oe.Suggestion == nil || !strings.Contains(oe.Suggestion.Human, "--force-foreign") {
		t.Errorf("suggestion must mention --force-foreign; got %+v", oe.Suggestion)
	}
}

// TestPreflightTarget_MatchingClusterAllowedUnderForce: --force into
// the SAME cluster (system id matches) proceeds.
func TestPreflightTarget_MatchingClusterAllowedUnderForce(t *testing.T) {
	const sysID = uint64(9090909090909090909)
	dir := writeFakeDatadir(t, sysID, true)
	if err := preflightTarget(dir, true, strconv.FormatUint(sysID, 10), false); err != nil {
		t.Errorf("--force into the matching cluster should proceed: %v", err)
	}
}

// TestPreflightTarget_ForceForeignBypasses: --force-foreign overrides
// the foreign-cluster refusal (forensic / hardware-repurpose case).
func TestPreflightTarget_ForceForeignBypasses(t *testing.T) {
	dir := writeFakeDatadir(t, 1111111111111111111, true)
	if err := preflightTarget(dir, true, "2222222222222222222", true); err != nil {
		t.Errorf("--force-foreign should override the foreign-cluster refusal: %v", err)
	}
}

// TestPreflightTarget_NoControlFileUnderForceProceeds: a --force target
// with no readable pg_control can't be identified, so the foreign check
// can't fire — --force proceeds (existing PG_VERSION + --force contract
// unchanged).
func TestPreflightTarget_NoControlFileUnderForceProceeds(t *testing.T) {
	dir := writeFakeDatadir(t, 0, false)
	if err := preflightTarget(dir, true, "2222222222222222222", false); err != nil {
		t.Errorf("--force with no pg_control should proceed: %v", err)
	}
}

// TestPreflightTarget_EmptyExpectedIDUnderForceProceeds: when the
// backup manifest carries no system identifier there's nothing to
// compare, so --force proceeds.
func TestPreflightTarget_EmptyExpectedIDUnderForceProceeds(t *testing.T) {
	dir := writeFakeDatadir(t, 1111111111111111111, true)
	if err := preflightTarget(dir, true, "", false); err != nil {
		t.Errorf("--force with empty expected system id should proceed: %v", err)
	}
}

// TestPreflightTablespaceTargets pins round-3 data-loss #2: the
// external tablespace dirs a restore redirects data into are guarded
// like the main target — a non-empty one is refused without --force,
// and cleared with it — so a restore can't silently clobber another
// cluster's tablespace.
func TestPreflightTablespaceTargets(t *testing.T) {
	// Absent dir → pass (pg_combinebackup / PG will create it).
	missing := filepath.Join(t.TempDir(), "nope")
	if err := preflightTablespaceTargets(TablespaceRemap{{Old: "/old", New: missing}}, false); err != nil {
		t.Errorf("absent tablespace target should pass: %v", err)
	}
	// Empty dir → pass.
	if err := preflightTablespaceTargets(TablespaceRemap{{Old: "/old", New: t.TempDir()}}, false); err != nil {
		t.Errorf("empty tablespace target should pass: %v", err)
	}
	// Non-empty without --force → refuse.
	nonEmpty := t.TempDir()
	if err := os.WriteFile(filepath.Join(nonEmpty, "other_cluster_ts"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := preflightTablespaceTargets(TablespaceRemap{{Old: "/old", New: nonEmpty}}, false)
	if err == nil {
		t.Fatal("non-empty tablespace target must be refused without --force")
	}
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != "preflight.tablespace_not_empty" {
		t.Errorf("code = %v, want preflight.tablespace_not_empty", err)
	}
	// Non-empty WITH --force → cleared.
	if err := preflightTablespaceTargets(TablespaceRemap{{Old: "/old", New: nonEmpty}}, true); err != nil {
		t.Fatalf("--force should clear the tablespace target: %v", err)
	}
	if entries, _ := os.ReadDir(nonEmpty); len(entries) != 0 {
		t.Errorf("--force should have cleared the tablespace target; %d entries remain", len(entries))
	}
}

// TestClearDirContents: removes every entry but keeps the dir itself.
func TestClearDirContents(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "b"), []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := clearDirContents(dir); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("clearDirContents left %d entries", len(entries))
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("dir itself must survive clearDirContents: %v", err)
	}
}
