// restore_tolsn_preview_test.go — regression tests for issue #99.
//
// Issue #99: `restore --preview --to-lsn <X>` silently dropped the
// flag.  The rendered preview always showed the BACKUP's stop_lsn
// under "Stop LSN / TLI:" regardless of what the operator passed,
// so a typo'd LSN (or an LSN BEFORE the backup's stop) looked
// indistinguishable from a correct one — and a real restore would
// then silently recover to end-of-WAL, producing a database at the
// wrong point in time with no error in sight.
//
// The fix wires Recovery through restore.PlanOptions, echoes the
// target back into the Plan body, and refuses unreachable LSN
// targets at preview time AND at real-restore time.  These tests
// pin that contract:
//
//  1. Preview surfaces the operator's --to-lsn / --to / --to-name
//     in the JSON body AND the text rendering.
//  2. Preview refuses --to-lsn that's BEFORE the backup's stop_lsn
//     (forward-WAL-replay reachability).
//  3. Non-preview restore refuses the same target via the same
//     structured error — preview and real-run must agree.
//  4. A reachable --to-lsn (>= stop_lsn) flows through cleanly.
//
// Any of these tests failing means an operator can again be in a
// 3am restore where their --to-lsn looks accepted but does nothing.
package cli_test

import (
	"context"
	stdjson "encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// TestRestorePreview_SurfacesToLSN: --preview --to-lsn must echo
// the target into the body so the operator can confirm it.  Before
// issue #99 the body had no "recovery" block — the operator could
// not distinguish a working --to-lsn from a no-op.
func TestRestorePreview_SurfacesToLSN(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("payload"))

	// The fixture's StopLSN is "0/30001A0"; pick a target AFTER
	// it so reachability passes and we exercise the echo path.
	target := "0/4000000"

	stdout, stderr, exit := runRestore(t,
		"db1", id,
		"--repo", w.repoURL,
		"--target", t.TempDir()+"/restored",
		"--to-lsn", target,
		"--preview",
		"-o", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("preview exit=%d\nstderr=%s\nstdout=%s", exit, stderr, stdout)
	}
	var res output.Result
	if err := stdjson.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("invalid JSON on stdout: %v\n%s", err, stdout)
	}
	body, _ := res.Result.(map[string]any)
	rec, ok := body["recovery"].(map[string]any)
	if !ok {
		t.Fatalf("preview body missing 'recovery' block; body=%v", body)
	}
	if got, _ := rec["target_lsn"].(string); got != target {
		t.Errorf("recovery.target_lsn = %q; want %q", got, target)
	}
}

// TestRestorePreview_SurfacesToLSN_Text: the human-readable text
// rendering must include a "Recovery target:" line — operators
// reading the default text output (not JSON) need the same
// confirmation the JSON body provides.
func TestRestorePreview_SurfacesToLSN_Text(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("payload-text"))
	target := "0/4000000"

	stdout, stderr, exit := runRestore(t,
		"db1", id,
		"--repo", w.repoURL,
		"--target", t.TempDir()+"/restored",
		"--to-lsn", target,
		"--preview",
		"-o", "text", // off-TTY default is JSON; force text for the renderer assertion
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("preview exit=%d\nstderr=%s\nstdout=%s", exit, stderr, stdout)
	}
	if !strings.Contains(stdout, "Recovery target:") {
		t.Errorf("text rendering missing 'Recovery target:' line:\n%s", stdout)
	}
	if !strings.Contains(stdout, target) {
		t.Errorf("text rendering missing target %q:\n%s", target, stdout)
	}
	// And the backup's stop_lsn must STILL be visible — the two are
	// distinct facts and an operator triaging a recovery needs both.
	if !strings.Contains(stdout, "Backup stop LSN:") {
		t.Errorf("text rendering missing 'Backup stop LSN:' line:\n%s", stdout)
	}
}

// TestRestorePreview_RefusesUnreachableToLSN: a target BEFORE the
// backup's stop_lsn cannot be reached by forward WAL replay.  PG
// would silently recover to end-of-WAL.  The preview must refuse
// up front with a structured restore.target_unreachable error.
//
// The bug in issue #99: the user's --to-lsn 0/3E0048F0 was BEFORE
// the backup's stop_lsn 0/3F000120.  Pre-fix preview accepted it
// silently; post-fix preview refuses with a clear error pointing
// at the relationship.
func TestRestorePreview_RefusesUnreachableToLSN(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("payload-unreach"))

	// Fixture StopLSN = "0/30001A0".  Pick something BEFORE it.
	bad := "0/3000000"

	stdout, errb, exit := runRestore(t,
		"db1", id,
		"--repo", w.repoURL,
		"--target", t.TempDir()+"/restored",
		"--to-lsn", bad,
		"--preview",
		"-o", "json",
	)
	if exit != int(output.ExitConflict) {
		t.Fatalf("expected ExitConflict(%d) for restore.target_unreachable; got %d\nstdout=%s\nstderr=%s",
			output.ExitConflict, exit, stdout, errb)
	}
	var res output.Result
	if err := stdjson.Unmarshal([]byte(errb), &res); err != nil {
		t.Fatalf("invalid JSON on stderr: %v\n%s", err, errb)
	}
	if res.Error.Code != "restore.target_unreachable" {
		t.Errorf("error.code = %q; want restore.target_unreachable", res.Error.Code)
	}
	// Operator-facing message must name BOTH the chosen target and
	// the backup's stop_lsn — that's the relationship the operator
	// needs to fix.
	if !strings.Contains(res.Error.Message, bad) {
		t.Errorf("error message missing target %q: %q", bad, res.Error.Message)
	}
	if !strings.Contains(res.Error.Message, "0/30001A0") {
		t.Errorf("error message missing backup stop_lsn 0/30001A0: %q", res.Error.Message)
	}
	// Suggestion must point at the recovery path (pick an earlier
	// backup OR a later LSN).
	if res.Error.Suggestion == nil || res.Error.Suggestion.Human == "" {
		t.Errorf("error must carry an operator suggestion: %+v", res.Error)
	}
}

// TestRestoreReal_RefusesUnreachableToLSN: a non-preview restore
// must enforce the same reachability gate.  An operator who runs
// the real restore without first running --preview must hit the
// same structured error rather than ending up with a wrong-point-
// in-time database.
//
// We use a fresh empty target so the restore reaches the gate
// without bouncing off pre-flight target-non-empty.
func TestRestoreReal_RefusesUnreachableToLSN(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("payload-real"))
	bad := "0/3000000" // BEFORE fixture StopLSN 0/30001A0

	_, errb, exit := runRestore(t,
		"db1", id,
		"--repo", w.repoURL,
		"--target", t.TempDir()+"/restored",
		"--to-lsn", bad,
		"-o", "json",
	)
	if exit != int(output.ExitConflict) {
		t.Fatalf("expected ExitConflict(%d) on real restore; got %d\nstderr=%s",
			output.ExitConflict, exit, errb)
	}
	var res output.Result
	if err := stdjson.Unmarshal([]byte(errb), &res); err != nil {
		t.Fatalf("invalid JSON on stderr: %v\n%s", err, errb)
	}
	if res.Error.Code != "restore.target_unreachable" {
		t.Errorf("real-restore error.code = %q; want restore.target_unreachable", res.Error.Code)
	}
}

// TestRestorePreview_AcceptsReachableToLSN: a --to-lsn AT or
// AFTER the backup's stop_lsn is reachable by WAL replay.  The
// preview must accept it (and surface it).  This is the
// counter-weight to the refusal test: without a happy-path check
// the refusal test could trivially pass by always refusing.
func TestRestorePreview_AcceptsReachableToLSN(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("payload-ok"))

	// Re-read the manifest so we know the exact stop_lsn from the
	// fixture.  Tying the test to the same constant the helper
	// uses would silently drift if the helper changed.
	m, err := w.store.Read(context.Background(), "db1", id, w.verifier)
	if err != nil {
		t.Fatal(err)
	}
	// Pick a target strictly after StopLSN — fixture is
	// "0/30001A0" so "0/40000000" is well past.
	target := "0/40000000"

	stdout, stderr, exit := runRestore(t,
		"db1", id,
		"--repo", w.repoURL,
		"--target", t.TempDir()+"/restored",
		"--to-lsn", target,
		"--preview",
		"-o", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("preview should accept reachable LSN (stop=%s, target=%s); exit=%d\nstderr=%s",
			m.StopLSN, target, exit, stderr)
	}
	if !strings.Contains(stdout, target) {
		t.Errorf("preview output missing target %q:\n%s", target, stdout)
	}
}

// TestRestorePreview_NoRecovery_BodyShapeUnchanged: a preview
// invoked WITHOUT any PITR flag must NOT emit a `recovery` JSON
// key.  The block is omitempty so external tooling parsing the
// preview body sees the same shape as before issue #99 landed —
// the additive change must remain truly additive.
func TestRestorePreview_NoRecovery_BodyShapeUnchanged(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("payload-noflags"))

	stdout, stderr, exit := runRestore(t,
		"db1", id,
		"--repo", w.repoURL,
		"--target", t.TempDir()+"/restored",
		"--preview",
		"-o", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("preview exit=%d\nstderr=%s", exit, stderr)
	}
	var res output.Result
	if err := stdjson.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout)
	}
	body, _ := res.Result.(map[string]any)
	if _, has := body["recovery"]; has {
		t.Errorf("plain preview must omit `recovery` key (omitempty contract); body=%v", body)
	}
}

// TestRestorePreview_NoRecovery_TextHasNoRecoveryLine: same
// contract on the text side — when no PITR target is set, the
// "Recovery target:" line must NOT appear.  Otherwise operators
// learn to ignore the line and the surface loses its signal
// value when there IS a real target.
func TestRestorePreview_NoRecovery_TextHasNoRecoveryLine(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("payload-text-noflags"))

	stdout, stderr, exit := runRestore(t,
		"db1", id,
		"--repo", w.repoURL,
		"--target", t.TempDir()+"/restored",
		"--preview",
		"-o", "text",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("preview exit=%d\nstderr=%s", exit, stderr)
	}
	if strings.Contains(stdout, "Recovery target:") {
		t.Errorf("text rendering should NOT show 'Recovery target:' when no PITR flag was set:\n%s", stdout)
	}
}

// TestRestorePreview_ExclusiveBoundary_Refuses: `--to-lsn <X>
// --to-exclusive` where X == backup.stop_lsn would land BEFORE
// the checkpoint (exclusive = stop just before the target).
// My initial #99 fix accepted this case silently because the
// gate only checked `target < stop`; the corrected gate must
// refuse with restore.target_unreachable.
func TestRestorePreview_ExclusiveBoundary_Refuses(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("payload-excl"))

	// Read the backup's actual stop_lsn so the test isn't tied
	// to the fixture's specific value (helper drift would
	// otherwise silently miss the boundary).
	m, err := w.store.Read(context.Background(), "db1", id, w.verifier)
	if err != nil {
		t.Fatal(err)
	}

	_, errb, exit := runRestore(t,
		"db1", id,
		"--repo", w.repoURL,
		"--target", t.TempDir()+"/restored",
		"--to-lsn", m.StopLSN, // exactly stop_lsn
		"--to-exclusive", // means "stop JUST BEFORE stop_lsn" → unreachable
		"--preview",
		"-o", "json",
	)
	if exit == int(output.ExitOK) {
		t.Fatal("exclusive --to-lsn == stop_lsn must refuse")
	}
	var res output.Result
	if err := stdjson.Unmarshal([]byte(errb), &res); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, errb)
	}
	if res.Error.Code != "restore.target_unreachable" {
		t.Errorf("code = %q; want restore.target_unreachable", res.Error.Code)
	}
}

// TestRestorePreview_IncrementalChain_RefusesUnreachable: the
// reachability gate fires for incremental leaves too.  Without
// this test, a refactor that moved the chain-restore dispatch
// before the gate would silently re-open issue #99 for chains.
func TestRestorePreview_IncrementalChain_RefusesUnreachable(t *testing.T) {
	w := newReadWorld(t)
	// Plant a full parent and an incremental leaf.  Both have
	// stop_lsn 0/30001A0 from the helper's defaults; the leaf is
	// what restore --preview sees.
	full := commitVerifiableBackup(t, w, "db1", 0, []byte("parent-full"))
	leaf := commitChainBackup(t, w, "db1", "I1", 2, full,
		backup.BackupTypeIncremental, 1, [][]byte{[]byte("inc-delta")})

	bad := "0/3000000" // BEFORE stop_lsn 0/30001A0

	_, errb, exit := runRestore(t,
		"db1", leaf,
		"--repo", w.repoURL,
		"--target", t.TempDir()+"/restored",
		"--to-lsn", bad,
		"--preview",
		"-o", "json",
	)
	if exit == int(output.ExitOK) {
		t.Fatal("incremental-chain preview must refuse unreachable --to-lsn")
	}
	var res output.Result
	if err := stdjson.Unmarshal([]byte(errb), &res); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, errb)
	}
	if res.Error.Code != "restore.target_unreachable" {
		t.Errorf("code = %q; want restore.target_unreachable", res.Error.Code)
	}
}

// TestRestorePreview_ToTime_TextRendering: the human-readable
// renderer must surface --to <time> as well — not just --to-lsn.
// The original fix added all three switch arms; this test pins
// the time arm so a renderer regression doesn't silently revert
// it for time-based PITR.
func TestRestorePreview_ToTime_TextRendering(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("payload-totime-text"))

	// Use a far-future time so the existing
	// conflict.backup_after_target check doesn't refuse — we
	// want the preview to PROCEED so the renderer runs.
	target := "2099-01-01T00:00:00Z"

	stdout, stderr, exit := runRestore(t,
		"db1", id,
		"--repo", w.repoURL,
		"--target", t.TempDir()+"/restored",
		"--to", target,
		"--preview",
		"-o", "text",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("preview exit=%d\nstderr=%s\nstdout=%s", exit, stderr, stdout)
	}
	if !strings.Contains(stdout, "Recovery target:") {
		t.Errorf("text rendering missing 'Recovery target:' line:\n%s", stdout)
	}
	if !strings.Contains(stdout, "time") {
		t.Errorf("text rendering should label the time branch:\n%s", stdout)
	}
}

// TestRestorePreview_ToName_TextRendering: same as above for
// --to-name.  Without this test a renderer regression in the
// name arm would slip through.
func TestRestorePreview_ToName_TextRendering(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("payload-toname-text"))

	stdout, stderr, exit := runRestore(t,
		"db1", id,
		"--repo", w.repoURL,
		"--target", t.TempDir()+"/restored",
		"--to-name", "before_drop",
		"--preview",
		"-o", "text",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("preview exit=%d\nstderr=%s\nstdout=%s", exit, stderr, stdout)
	}
	if !strings.Contains(stdout, "Recovery target:") {
		t.Errorf("text rendering missing 'Recovery target:' line:\n%s", stdout)
	}
	if !strings.Contains(stdout, "before_drop") {
		t.Errorf("text rendering missing the named target:\n%s", stdout)
	}
}

// TestRestorePreview_ToTime_PreBackup_RefusedByExistingGate: an
// operator who passes --to <T> where T predates the backup's
// StoppedAt is refused by the existing conflict.backup_after_target
// gate (because forward replay against THIS backup can't reach
// T).  This test pins that posture from the preview path so a
// refactor of the preview pipeline that bypasses the time-target
// gate is caught.
func TestRestorePreview_ToTime_PreBackup_RefusedByExistingGate(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("payload-pre-time"))

	m, err := w.store.Read(context.Background(), "db1", id, w.verifier)
	if err != nil {
		t.Fatal(err)
	}
	// 1 minute BEFORE the backup's StoppedAt — unreachable by
	// forward replay against THIS backup.
	target := m.StoppedAt.Add(-time.Minute).UTC().Format(time.RFC3339)

	_, errb, exit := runRestore(t,
		"db1", id,
		"--repo", w.repoURL,
		"--target", t.TempDir()+"/restored",
		"--to", target,
		"--preview",
		"-o", "json",
	)
	if exit == int(output.ExitOK) {
		t.Fatal("preview with --to before backup must refuse")
	}
	var res output.Result
	if err := stdjson.Unmarshal([]byte(errb), &res); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, errb)
	}
	if res.Error.Code != "conflict.backup_after_target" {
		t.Errorf("code = %q; want conflict.backup_after_target", res.Error.Code)
	}
}

// TestRestorePreview_FullPITRSurface: every PITR flag combo
// (--to-lsn, --to-action, --to-timeline, --to-exclusive) must
// flow through buildRecovery into the preview body so the
// operator can confirm the WHOLE recovery configuration before
// committing to the restore.  Without this test, a refactor
// that drops one of the side fields silently degrades the
// operator's preview confidence.
func TestRestorePreview_FullPITRSurface(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("payload-full-surface"))

	// Reachable LSN well past the fixture's stop_lsn 0/30001A0.
	target := "0/40000000"

	stdout, stderr, exit := runRestore(t,
		"db1", id,
		"--repo", w.repoURL,
		"--target", t.TempDir()+"/restored",
		"--to-lsn", target,
		"--to-action", "shutdown",
		"--to-timeline", "3",
		"--to-exclusive", // Inclusive=false
		"--preview",
		"-o", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("preview exit=%d\nstderr=%s", exit, stderr)
	}
	var res output.Result
	if err := stdjson.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout)
	}
	body, _ := res.Result.(map[string]any)
	rec, ok := body["recovery"].(map[string]any)
	if !ok {
		t.Fatalf("body missing recovery block: %v", body)
	}
	// Every operator-visible field must be present and equal to
	// what we passed.
	checks := map[string]any{
		"target_lsn": target,
		"action":     "shutdown",
		"timeline":   "3",
		"inclusive":  false,
	}
	for k, want := range checks {
		got := rec[k]
		if got != want {
			t.Errorf("recovery.%s = %v (%T); want %v (%T)", k, got, got, want, want)
		}
	}
}

// TestRestorePreview_DefaultsSurface: when the operator passes
// only --to-lsn, the implicit defaults (inclusive=true,
// action="pause", timeline="latest") must surface in the
// preview body.  Operators reading the preview to confirm "what
// will PG actually do" need to see the DEFAULTS as well as the
// explicit values — otherwise an inherited default that
// disagrees with operator intent goes invisible.
func TestRestorePreview_DefaultsSurface(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("payload-defaults"))

	stdout, stderr, exit := runRestore(t,
		"db1", id,
		"--repo", w.repoURL,
		"--target", t.TempDir()+"/restored",
		"--to-lsn", "0/40000000",
		"--preview",
		"-o", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("preview exit=%d\nstderr=%s", exit, stderr)
	}
	var res output.Result
	if err := stdjson.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout)
	}
	body, _ := res.Result.(map[string]any)
	rec, ok := body["recovery"].(map[string]any)
	if !ok {
		t.Fatalf("body missing recovery block: %v", body)
	}
	// CLI's buildRecovery defaults: inclusive=true (PG default),
	// action="pause" (CLI flag default), timeline="latest" (CLI
	// flag default).
	if rec["inclusive"] != true {
		t.Errorf("default inclusive should be true; got %v", rec["inclusive"])
	}
	if rec["action"] != "pause" {
		t.Errorf("default action should be 'pause'; got %v", rec["action"])
	}
	if rec["timeline"] != "latest" {
		t.Errorf("default timeline should be 'latest'; got %v", rec["timeline"])
	}
}

// TestRestorePreview_ConflictingTargets: --to-lsn + --to in the
// same invocation already had a CLI-layer test
// (usage.conflicting_targets); pin the same posture for
// --to-lsn + --to-name and the triple-set case so the
// at-most-one rule is fully gated.
func TestRestorePreview_ConflictingTargets(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("payload-conflict"))

	cases := []struct {
		name string
		args []string
	}{
		{
			name: "lsn + name",
			args: []string{"--to-lsn", "0/40000000", "--to-name", "x"},
		},
		{
			name: "time + name",
			args: []string{"--to", "2099-01-01T00:00:00Z", "--to-name", "x"},
		},
		{
			name: "all three",
			args: []string{"--to-lsn", "0/40000000", "--to", "2099-01-01T00:00:00Z", "--to-name", "x"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{
				"db1", id,
				"--repo", w.repoURL,
				"--target", t.TempDir() + "/restored",
				"--preview",
				"-o", "json",
			}
			args = append(args, tc.args...)
			_, errb, exit := runRestore(t, args...)
			if exit != int(output.ExitMisuse) {
				t.Fatalf("expected ExitMisuse; got %d\nstderr=%s", exit, errb)
			}
			var res output.Result
			if err := stdjson.Unmarshal([]byte(errb), &res); err != nil {
				t.Fatalf("invalid JSON: %v\n%s", err, errb)
			}
			if res.Error.Code != "usage.conflicting_targets" {
				t.Errorf("code = %q; want usage.conflicting_targets", res.Error.Code)
			}
		})
	}
}

// TestRestorePreview_SurfacesToName: --to-name flows through the
// same echo path.  Time/name targets can't be statically range-
// checked (PG resolves them to an LSN at recovery time), so the
// echo is the operator's only feedback that the flag took.
func TestRestorePreview_SurfacesToName(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("payload-name"))

	stdout, stderr, exit := runRestore(t,
		"db1", id,
		"--repo", w.repoURL,
		"--target", t.TempDir()+"/restored",
		"--to-name", "before_disaster",
		"--preview",
		"-o", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("preview exit=%d\nstderr=%s\nstdout=%s", exit, stderr, stdout)
	}
	var res output.Result
	if err := stdjson.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout)
	}
	body, _ := res.Result.(map[string]any)
	rec, ok := body["recovery"].(map[string]any)
	if !ok {
		t.Fatalf("body missing recovery block: %v", body)
	}
	if got, _ := rec["target_name"].(string); got != "before_disaster" {
		t.Errorf("recovery.target_name = %q; want before_disaster", got)
	}
}
