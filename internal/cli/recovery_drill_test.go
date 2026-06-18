package cli_test

import (
	stdjson "encoding/json"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// drillView mirrors the v1 contract's top-level shape.  Pulls
// only the fields the CLI tests assert on.
type drillView struct {
	Schema             string `json:"schema"`
	URL                string `json:"url"`
	Deployment         string `json:"deployment"`
	BackupID           string `json:"backup_id"`
	Verdict            string `json:"verdict"`
	RTOActualSeconds   int64  `json:"rto_actual_seconds"`
	RTOEstimateSeconds int64  `json:"rto_estimate_seconds"`
	Phases             []struct {
		Name string `json:"name"`
		OK   bool   `json:"ok"`
	} `json:"phases"`
	Issues []struct {
		Severity string `json:"severity"`
		Code     string `json:"code"`
	} `json:"issues"`
}

// TestRecoveryDrill_RequiresRepo: --repo is mandatory.
func TestRecoveryDrill_RequiresRepo(t *testing.T) {
	_ = newReadWorld(t)
	_, errb, exit := runCLI(t, "recovery", "drill", "db1", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.missing_flag") {
		t.Errorf("expected usage.missing_flag:\n%s", errb)
	}
}

// TestRecoveryDrill_BadFormat
func TestRecoveryDrill_BadFormat(t *testing.T) {
	w := newReadWorld(t)
	_, errb, exit := runCLI(t, "recovery", "drill", "db1",
		"--repo", w.repoURL, "--format", "csv", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.bad_flag") {
		t.Errorf("expected usage.bad_flag:\n%s", errb)
	}
}

// TestRecoveryDrill_NegativeRTO
func TestRecoveryDrill_NegativeRTO(t *testing.T) {
	w := newReadWorld(t)
	_, errb, exit := runCLI(t, "recovery", "drill", "db1",
		"--repo", w.repoURL, "--rto-seconds", "-1", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.bad_flag") {
		t.Errorf("expected usage.bad_flag:\n%s", errb)
	}
}

// TestRecoveryDrill_SkipFlagsConflict: --skip-verify and
// --allow-skip-verify are mutually exclusive.
func TestRecoveryDrill_SkipFlagsConflict(t *testing.T) {
	w := newReadWorld(t)
	_, errb, exit := runCLI(t, "recovery", "drill", "db1",
		"--repo", w.repoURL,
		"--skip-verify", "--allow-skip-verify",
		"-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.bad_flag") {
		t.Errorf("expected usage.bad_flag:\n%s", errb)
	}
}

// TestRecoveryDrill_NoBackups: a deployment with no backups
// produces a Fail verdict + critical issue.  Drill with
// --skip-verify (no Docker) so the test is fast.
func TestRecoveryDrill_NoBackups(t *testing.T) {
	w := newReadWorld(t)
	stdout, errb, exit := runCLI(t, "recovery", "drill", "db1",
		"--repo", w.repoURL, "--skip-verify", "-o", "json")
	if exit != int(output.ExitVerifyFailed) {
		t.Errorf("exit = %d, want ExitVerifyFailed (%d) for fail verdict",
			exit, output.ExitVerifyFailed)
	}
	if !strings.Contains(errb, "verify.drill_failed") {
		t.Errorf("expected verify.drill_failed in stderr:\n%s", errb)
	}
	// Body still rendered to stdout (dual-stream pattern).
	var any map[string]any
	if err := unmarshalDrillBody(stdout, &any); err != nil {
		t.Errorf("body decode: %v", err)
	}
}

// TestRecoveryDrill_HappyPath_WithSkipVerify: a valid backup +
// --skip-verify yields a partial verdict (and exit 0).
func TestRecoveryDrill_HappyPath_WithSkipVerify(t *testing.T) {
	w := newReadWorld(t)
	commitVerifiableBackup(t, w, "db1", 0, []byte("dummy-payload"))

	stdout, _, exit := runCLI(t, "recovery", "drill", "db1",
		"--repo", w.repoURL, "--skip-verify", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d, want OK (partial verdict)\n%s", exit, stdout)
	}
	var view drillView
	bodyOf(t, stdout, &view)
	if view.Schema != "pg_hardstorage.recovery.drill.v1" {
		t.Errorf("Schema = %q", view.Schema)
	}
	if view.Verdict != "partial" {
		t.Errorf("Verdict = %q, want partial", view.Verdict)
	}
	if view.BackupID == "" {
		t.Errorf("BackupID should be populated")
	}
	// Phases at minimum: pick + prepare + restore + teardown.
	if len(view.Phases) < 4 {
		t.Errorf("Phases = %v, want >=4", view.Phases)
	}
}

// TestRecoveryDrill_PicksLatest: with multiple backups, drill
// picks the newest by default.
func TestRecoveryDrill_PicksLatest(t *testing.T) {
	w := newReadWorld(t)
	commitVerifiableBackup(t, w, "db1", 0, []byte("first"))
	expectLatest := commitVerifiableBackup(t, w, "db1", 5, []byte("newer"))

	stdout, _, exit := runCLI(t, "recovery", "drill", "db1",
		"--repo", w.repoURL, "--skip-verify", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	var view drillView
	bodyOf(t, stdout, &view)
	if view.BackupID != expectLatest {
		t.Errorf("BackupID = %q, want %q", view.BackupID, expectLatest)
	}
}

// TestRecoveryDrill_ExplicitBackupID
func TestRecoveryDrill_ExplicitBackupID(t *testing.T) {
	w := newReadWorld(t)
	older := commitVerifiableBackup(t, w, "db1", 0, []byte("older"))
	commitVerifiableBackup(t, w, "db1", 5, []byte("newer"))

	stdout, _, exit := runCLI(t, "recovery", "drill", "db1",
		"--repo", w.repoURL, "--skip-verify",
		"--backup-id", older, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	var view drillView
	bodyOf(t, stdout, &view)
	if view.BackupID != older {
		t.Errorf("BackupID = %q, want %q", view.BackupID, older)
	}
}

// TestRecoveryDrill_RTOEstimateSurfaced
func TestRecoveryDrill_RTOEstimateSurfaced(t *testing.T) {
	w := newReadWorld(t)
	commitVerifiableBackup(t, w, "db1", 0, []byte("payload"))

	stdout, _, exit := runCLI(t, "recovery", "drill", "db1",
		"--repo", w.repoURL, "--skip-verify",
		"--rto-seconds", "300", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	var view drillView
	bodyOf(t, stdout, &view)
	if view.RTOEstimateSeconds != 300 {
		t.Errorf("RTOEstimateSeconds = %d, want 300", view.RTOEstimateSeconds)
	}
}

// TestRecoveryDrill_TextFormat
func TestRecoveryDrill_TextFormat(t *testing.T) {
	w := newReadWorld(t)
	commitVerifiableBackup(t, w, "db1", 0, []byte("payload"))

	stdout, _, exit := runCLI(t, "recovery", "drill", "db1",
		"--repo", w.repoURL, "--skip-verify", "-o", "text")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	for _, want := range []string{
		"recovery drill",
		"Verdict: PARTIAL",
		"Phases:",
		"pick",
		"restore",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("text output missing %q:\n%s", want, stdout)
		}
	}
}

// TestRecoveryDrill_MarkdownFormat
func TestRecoveryDrill_MarkdownFormat(t *testing.T) {
	w := newReadWorld(t)
	commitVerifiableBackup(t, w, "db1", 0, []byte("payload"))

	stdout, _, exit := runCLI(t, "recovery", "drill", "db1",
		"--repo", w.repoURL, "--skip-verify",
		"--format", "markdown", "-o", "text")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	for _, want := range []string{
		"# pg_hardstorage recovery drill",
		"## Verdict",
		"## Phases",
		"## RTO",
		"## Restore detail",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("Markdown output missing %q:\n%s", want, stdout)
		}
	}
}

// TestRecoveryDrill_HelpDiscoverable
func TestRecoveryDrill_HelpDiscoverable(t *testing.T) {
	stdout, _, _ := runCLI(t, "recovery", "drill", "--help")
	for _, want := range []string{
		"--backup-id",
		"--pg-major",
		"--image",
		"--temp-base",
		"--keep",
		"--allow-skip-verify",
		"--skip-verify",
		"--rto-seconds",
		"--format",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("recovery drill --help missing %q:\n%s", want, stdout)
		}
	}
	stdout, _, _ = runCLI(t, "recovery", "--help")
	if !strings.Contains(stdout, "drill") {
		t.Errorf("recovery --help missing drill subcommand:\n%s", stdout)
	}
}

// TestRecoveryDrill_KeepFlag: --keep leaves the temp dir + the
// path is reported.  Cleanup happens via t.TempDir() since we
// passed --temp-base into one.
func TestRecoveryDrill_KeepFlag(t *testing.T) {
	w := newReadWorld(t)
	commitVerifiableBackup(t, w, "db1", 0, []byte("payload"))

	tempBase := t.TempDir()
	stdout, _, exit := runCLI(t, "recovery", "drill", "db1",
		"--repo", w.repoURL, "--skip-verify",
		"--temp-base", tempBase, "--keep", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	if !strings.Contains(stdout, "target_dir") {
		t.Errorf("--keep should surface target_dir in body:\n%s", stdout)
	}
	// The directory existed when the drill returned; it gets
	// auto-cleaned when t.TempDir's cleanup fires.
}

// TestRecoveryDrill_NonExistentBackupID: explicit --backup-id
// pointing at nothing → fail verdict.
func TestRecoveryDrill_NonExistentBackupID(t *testing.T) {
	w := newReadWorld(t)
	commitVerifiableBackup(t, w, "db1", 0, []byte("payload"))

	stdout, errb, exit := runCLI(t, "recovery", "drill", "db1",
		"--repo", w.repoURL, "--skip-verify",
		"--backup-id", "db1.full.NEVER", "-o", "json")
	if exit != int(output.ExitVerifyFailed) {
		t.Errorf("exit = %d, want ExitVerifyFailed", exit)
	}
	if !strings.Contains(errb, "verify.drill_failed") {
		t.Errorf("expected verify.drill_failed in stderr:\n%s", errb)
	}
	// Body should still render a fail verdict.
	var view drillView
	if err := unmarshalDrillBody(stdout, &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view.Verdict != "fail" {
		t.Errorf("Verdict = %q, want fail", view.Verdict)
	}
}

// unmarshalDrillBody is a permissive unmarshaller for the
// dual-stream output (success body emitted alongside an error).
// Mirrors unmarshalResultBody in kms_verify_test.go.
func unmarshalDrillBody(raw string, into any) error {
	var env struct {
		Result *stdjson.RawMessage `json:"result"`
	}
	if err := stdjson.Unmarshal([]byte(raw), &env); err != nil {
		return err
	}
	if env.Result == nil {
		return nil
	}
	return stdjson.Unmarshal(*env.Result, into)
}
