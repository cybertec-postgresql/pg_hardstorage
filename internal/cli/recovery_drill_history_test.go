package cli_test

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// drillHistoryView mirrors the v1 contract's top-level shape.
type drillHistoryView struct {
	URL        string `json:"url"`
	Deployment string `json:"deployment"`
	Verdict    string `json:"verdict"`
	Summary    *struct {
		Total            int     `json:"total"`
		PassCount        int     `json:"pass_count"`
		PartialCount     int     `json:"partial_count"`
		FailCount        int     `json:"fail_count"`
		PassPercent      float64 `json:"pass_percent"`
		LatestVerdict    string  `json:"latest_verdict"`
		VerdictTrend     string  `json:"verdict_trend"`
		RTOMinSeconds    int64   `json:"rto_min_seconds"`
		RTOMaxSeconds    int64   `json:"rto_max_seconds"`
		RTOMedianSeconds int64   `json:"rto_median_seconds"`
		RTOMeanSeconds   int64   `json:"rto_mean_seconds"`
	} `json:"summary"`
	Entries []struct {
		Deployment       string `json:"deployment"`
		BackupID         string `json:"backup_id"`
		Verdict          string `json:"verdict"`
		RTOActualSeconds int64  `json:"rto_actual_seconds"`
		Operator         string `json:"operator"`
	} `json:"entries"`
}

// runDrillsForHistory plants N drills against a real readWorld
// repo via the existing recovery drill --skip-verify path.  Used
// by the history CLI tests below to populate recovery/drills/.
func runDrillsForHistory(t *testing.T, w *readWorld, deployment string, n int) {
	t.Helper()
	commitVerifiableBackup(t, w, deployment, 0, []byte("payload"))
	for i := 0; i < n; i++ {
		_, _, exit := runCLI(t, "recovery", "drill", deployment,
			"--repo", w.repoURL, "--skip-verify",
			"--operator", "test-runner", "-o", "json")
		if exit != int(output.ExitOK) {
			t.Fatalf("drill #%d exit = %d", i, exit)
		}
	}
}

// TestRecoveryDrillHistory_RequiresRepo
func TestRecoveryDrillHistory_RequiresRepo(t *testing.T) {
	_ = newReadWorld(t)
	_, errb, exit := runCLI(t, "recovery", "drill", "history", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.missing_flag") {
		t.Errorf("expected usage.missing_flag:\n%s", errb)
	}
}

// TestRecoveryDrillHistory_BadVerdict
func TestRecoveryDrillHistory_BadVerdict(t *testing.T) {
	w := newReadWorld(t)
	_, errb, exit := runCLI(t, "recovery", "drill", "history",
		"--repo", w.repoURL, "--verdict", "weird", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.bad_flag") {
		t.Errorf("expected usage.bad_flag:\n%s", errb)
	}
}

// TestRecoveryDrillHistory_BadFormat
func TestRecoveryDrillHistory_BadFormat(t *testing.T) {
	w := newReadWorld(t)
	_, errb, exit := runCLI(t, "recovery", "drill", "history",
		"--repo", w.repoURL, "--format", "csv", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.bad_flag") {
		t.Errorf("expected usage.bad_flag:\n%s", errb)
	}
}

// TestRecoveryDrillHistory_NegativeLimit
func TestRecoveryDrillHistory_NegativeLimit(t *testing.T) {
	w := newReadWorld(t)
	_, errb, exit := runCLI(t, "recovery", "drill", "history",
		"--repo", w.repoURL, "--limit", "-1", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.bad_flag") {
		t.Errorf("expected usage.bad_flag:\n%s", errb)
	}
}

// TestRecoveryDrillHistory_BadSinceUntil
func TestRecoveryDrillHistory_BadSinceUntil(t *testing.T) {
	w := newReadWorld(t)
	_, errb, exit := runCLI(t, "recovery", "drill", "history",
		"--repo", w.repoURL, "--since", "yesterday-ish", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.bad_flag") {
		t.Errorf("expected usage.bad_flag:\n%s", errb)
	}
}

// TestRecoveryDrillHistory_PositionalAndFlagConflict
func TestRecoveryDrillHistory_PositionalAndFlagConflict(t *testing.T) {
	w := newReadWorld(t)
	_, errb, exit := runCLI(t, "recovery", "drill", "history",
		"db1", "--deployment", "db2",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.bad_flag") {
		t.Errorf("expected usage.bad_flag:\n%s", errb)
	}
}

// TestRecoveryDrillHistory_EmptyRepo: no drills → empty summary
// + empty entries.
func TestRecoveryDrillHistory_EmptyRepo(t *testing.T) {
	w := newReadWorld(t)
	stdout, _, exit := runCLI(t, "recovery", "drill", "history",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	var view drillHistoryView
	bodyOf(t, stdout, &view)
	if view.Summary == nil || view.Summary.Total != 0 {
		t.Errorf("expected zero summary; got %+v", view.Summary)
	}
}

// TestRecoveryDrillHistory_HappyPath: run a drill, then read
// the history.
func TestRecoveryDrillHistory_HappyPath(t *testing.T) {
	w := newReadWorld(t)
	runDrillsForHistory(t, w, "db1", 1)

	stdout, _, exit := runCLI(t, "recovery", "drill", "history", "db1",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\n%s", exit, stdout)
	}
	var view drillHistoryView
	bodyOf(t, stdout, &view)
	if view.Summary == nil || view.Summary.Total != 1 {
		t.Errorf("Summary.Total = %d, want 1\n%+v", view.Summary.Total, view.Summary)
	}
	if len(view.Entries) != 1 {
		t.Errorf("Entries = %d, want 1", len(view.Entries))
	}
	if view.Entries[0].Operator != "test-runner" {
		t.Errorf("Operator = %q, want test-runner", view.Entries[0].Operator)
	}
}

// TestRecoveryDrillHistory_DeploymentFilter
func TestRecoveryDrillHistory_DeploymentFilter(t *testing.T) {
	w := newReadWorld(t)
	runDrillsForHistory(t, w, "db1", 1)
	runDrillsForHistory(t, w, "db2", 1)

	stdout, _, exit := runCLI(t, "recovery", "drill", "history", "db1",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	var view drillHistoryView
	bodyOf(t, stdout, &view)
	if view.Summary.Total != 1 {
		t.Errorf("filtered Total = %d, want 1", view.Summary.Total)
	}
	for _, e := range view.Entries {
		if e.Deployment != "db1" {
			t.Errorf("filter leaked: %+v", e)
		}
	}
}

// TestRecoveryDrillHistory_Summarize: --summarize drops the
// per-entry slice.
func TestRecoveryDrillHistory_Summarize(t *testing.T) {
	w := newReadWorld(t)
	runDrillsForHistory(t, w, "db1", 3)

	stdout, _, exit := runCLI(t, "recovery", "drill", "history",
		"--repo", w.repoURL, "--summarize", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	var view drillHistoryView
	bodyOf(t, stdout, &view)
	if view.Summary == nil || view.Summary.Total != 3 {
		t.Errorf("Summary = %+v, want Total=3", view.Summary)
	}
	if len(view.Entries) != 0 {
		t.Errorf("Entries = %d, want 0 (--summarize)", len(view.Entries))
	}
}

// TestRecoveryDrillHistory_NoHistoryFlagOnDrill: --no-history
// suppresses the auto-persist.
func TestRecoveryDrillHistory_NoHistoryFlagOnDrill(t *testing.T) {
	w := newReadWorld(t)
	commitVerifiableBackup(t, w, "db1", 0, []byte("payload"))
	_, _, exit := runCLI(t, "recovery", "drill", "db1",
		"--repo", w.repoURL, "--skip-verify", "--no-history", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("drill exit = %d", exit)
	}
	stdout, _, exit := runCLI(t, "recovery", "drill", "history",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("history exit = %d", exit)
	}
	var view drillHistoryView
	bodyOf(t, stdout, &view)
	if view.Summary.Total != 0 {
		t.Errorf("--no-history should not persist; Total = %d", view.Summary.Total)
	}
}

// TestRecoveryDrillHistory_VerdictFilter
func TestRecoveryDrillHistory_VerdictFilter(t *testing.T) {
	w := newReadWorld(t)
	// Plant one partial drill (--skip-verify yields partial).
	runDrillsForHistory(t, w, "db1", 1)

	// Filter for pass — should be empty.
	stdout, _, exit := runCLI(t, "recovery", "drill", "history",
		"--repo", w.repoURL, "--verdict", "pass", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	var view drillHistoryView
	bodyOf(t, stdout, &view)
	if view.Summary.Total != 0 {
		t.Errorf("verdict=pass filter; Total = %d, want 0 (drills are partial)", view.Summary.Total)
	}

	// Filter for partial — should be 1.
	stdout, _, exit = runCLI(t, "recovery", "drill", "history",
		"--repo", w.repoURL, "--verdict", "partial", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	bodyOf(t, stdout, &view)
	if view.Summary.Total != 1 {
		t.Errorf("verdict=partial; Total = %d, want 1", view.Summary.Total)
	}
}

// TestRecoveryDrillHistory_TextFormat
func TestRecoveryDrillHistory_TextFormat(t *testing.T) {
	w := newReadWorld(t)
	runDrillsForHistory(t, w, "db1", 2)

	stdout, _, exit := runCLI(t, "recovery", "drill", "history",
		"--repo", w.repoURL, "-o", "text")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	for _, want := range []string{
		"drill history",
		"Total runs:",
		"Pass / Part / Fail:",
		"Pass percent:",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("text output missing %q:\n%s", want, stdout)
		}
	}
}

// TestRecoveryDrillHistory_MarkdownFormat
func TestRecoveryDrillHistory_MarkdownFormat(t *testing.T) {
	w := newReadWorld(t)
	runDrillsForHistory(t, w, "db1", 1)

	stdout, _, exit := runCLI(t, "recovery", "drill", "history",
		"--repo", w.repoURL, "--format", "markdown", "-o", "text")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	for _, want := range []string{
		"# pg_hardstorage drill history",
		"## Summary",
		"## Drills",
		"db1",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("Markdown output missing %q:\n%s", want, stdout)
		}
	}
}

// TestRecoveryDrillHistory_HelpDiscoverable
func TestRecoveryDrillHistory_HelpDiscoverable(t *testing.T) {
	stdout, _, _ := runCLI(t, "recovery", "drill", "--help")
	if !strings.Contains(stdout, "history") {
		t.Errorf("recovery drill --help missing history subcommand:\n%s", stdout)
	}
	stdout, _, _ = runCLI(t, "recovery", "drill", "history", "--help")
	for _, want := range []string{
		"--verdict", "--since", "--until", "--limit",
		"--reverse", "--summarize", "--format",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("recovery drill history --help missing %q:\n%s", want, stdout)
		}
	}
	stdout, _, _ = runCLI(t, "recovery", "drill", "--help")
	for _, want := range []string{
		"--no-history",
		"--operator",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("recovery drill --help missing %q:\n%s", want, stdout)
		}
	}
}

// TestRecoveryDrillHistory_LimitWorks
func TestRecoveryDrillHistory_LimitWorks(t *testing.T) {
	w := newReadWorld(t)
	runDrillsForHistory(t, w, "db1", 5)

	stdout, _, exit := runCLI(t, "recovery", "drill", "history",
		"--repo", w.repoURL, "--limit", "2", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	var view drillHistoryView
	bodyOf(t, stdout, &view)
	if len(view.Entries) != 2 {
		t.Errorf("Entries = %d, want 2 (--limit=2)", len(view.Entries))
	}
	// Summary still computes against everything matched.  With
	// limit=2, the summary.Total is also 2 (the filter caps the
	// list passed to Summarize).
	if view.Summary.Total != 2 {
		t.Errorf("Summary.Total = %d, want 2", view.Summary.Total)
	}
}
