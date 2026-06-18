package cli_test

import (
	"context"
	stdjson "encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// complianceReportView mirrors the v1 contract's top-level shape.
// Only the fields the CLI tests assert on are pulled in.
type complianceReportView struct {
	Schema           string `json:"schema"`
	URL              string `json:"url"`
	DeploymentFilter string `json:"deployment_filter"`
	Since            string `json:"since"`
	Until            string `json:"until"`
	Backups          *struct {
		TotalCommitted int            `json:"total_committed"`
		ByType         map[string]int `json:"by_type"`
	} `json:"backups"`
	Encryption *struct {
		EncryptedCount   int     `json:"encrypted_count"`
		UnencryptedCount int     `json:"unencrypted_count"`
		CoveragePercent  float64 `json:"coverage_percent"`
	} `json:"encryption"`
	Verification *struct {
		TotalRuns int `json:"total_runs"`
	} `json:"verification"`
	KEKLifecycle *struct {
		RotationsAttempted int `json:"rotations_attempted"`
	} `json:"kek_lifecycle"`
	Approvals *struct {
		DestructiveOps int `json:"destructive_ops_executed"`
	} `json:"approvals"`
	Holds *struct {
		HoldsAdded int `json:"holds_added"`
	} `json:"holds"`
	Replicas *struct {
		WindowedPrimaries int `json:"windowed_primaries"`
	} `json:"replicas"`
	Chain *struct {
		EventsInWindow int `json:"events_in_window"`
	} `json:"chain"`
	WORM *struct {
		Active bool `json:"active"`
	} `json:"worm"`
}

// TestCompliance_RequiresRepo: --repo or positional URL is required.
func TestCompliance_RequiresRepo(t *testing.T) {
	_ = newReadWorld(t)
	_, errb, exit := runCLI(t, "compliance", "report", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.missing_flag") {
		t.Errorf("expected usage.missing_flag:\n%s", errb)
	}
}

// TestCompliance_BadFormat: --format must be json or markdown.
func TestCompliance_BadFormat(t *testing.T) {
	w := newReadWorld(t)
	_, errb, exit := runCLI(t, "compliance", "report",
		"--repo", w.repoURL, "--format", "csv", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.bad_flag") {
		t.Errorf("expected usage.bad_flag:\n%s", errb)
	}
}

// TestCompliance_BadSince: malformed --since surfaces usage.bad_flag.
func TestCompliance_BadSince(t *testing.T) {
	w := newReadWorld(t)
	_, errb, exit := runCLI(t, "compliance", "report",
		"--repo", w.repoURL, "--since", "yesterday-ish", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.bad_flag") {
		t.Errorf("expected usage.bad_flag:\n%s", errb)
	}
}

// TestCompliance_PositionalAndFlagConflict: both forms disagreeing.
func TestCompliance_PositionalAndFlagConflict(t *testing.T) {
	w := newReadWorld(t)
	_, errb, exit := runCLI(t, "compliance", "report",
		w.repoURL+"-other", "--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.repo_conflict") {
		t.Errorf("expected usage.repo_conflict:\n%s", errb)
	}
}

// TestCompliance_EmptyRepo: a fresh repo produces a clean report
// with zero counts.
func TestCompliance_EmptyRepo(t *testing.T) {
	w := newReadWorld(t)
	stdout, _, exit := runCLI(t, "compliance", "report",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\n%s", exit, stdout)
	}
	var view complianceReportView
	bodyOf(t, stdout, &view)
	if view.Schema != "pg_hardstorage.compliance.v1" {
		t.Errorf("Schema = %q", view.Schema)
	}
	if view.URL != w.repoURL {
		t.Errorf("URL = %q", view.URL)
	}
	if view.Backups == nil || view.Backups.TotalCommitted != 0 {
		t.Errorf("Backups = %+v", view.Backups)
	}
}

// TestCompliance_BackupsCounted: a committed backup shows up in
// the BackupSection.
func TestCompliance_BackupsCounted(t *testing.T) {
	w := newReadWorld(t)
	commitVerifiableBackup(t, w, "db1", 0, []byte("a"))

	stdout, _, exit := runCLI(t, "compliance", "report",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\n%s", exit, stdout)
	}
	var view complianceReportView
	bodyOf(t, stdout, &view)
	if view.Backups == nil || view.Backups.TotalCommitted != 1 {
		t.Errorf("Backups.TotalCommitted = %+v, want 1", view.Backups)
	}
}

// TestCompliance_AuditEvents_Surface: kms.rotate audit events show
// up in KEKLifecycle; backup.delete shows up in Approvals.
func TestCompliance_AuditEvents_Surface(t *testing.T) {
	w := newReadWorld(t)
	store := audit.NewStore(w.sp)
	now := time.Now().UTC()
	if err := store.Append(context.Background(), &audit.Event{
		Action:    "kms.rotate",
		Timestamp: now.Add(-1 * time.Hour),
		Body: map[string]any{
			"old_kek_ref": "tenant:v1",
			"new_kek_ref": "tenant:v2",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(context.Background(), &audit.Event{
		Action:    "backup.delete",
		Subject:   audit.Subject{Deployment: "db1"},
		Timestamp: now.Add(-30 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	stdout, _, exit := runCLI(t, "compliance", "report",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\n%s", exit, stdout)
	}
	var view complianceReportView
	bodyOf(t, stdout, &view)
	if view.KEKLifecycle == nil || view.KEKLifecycle.RotationsAttempted != 1 {
		t.Errorf("RotationsAttempted = %+v", view.KEKLifecycle)
	}
	if view.Approvals == nil || view.Approvals.DestructiveOps != 1 {
		t.Errorf("DestructiveOps = %+v", view.Approvals)
	}
}

// TestCompliance_DeploymentFilter: only the named deployment shows
// in windowed sections.
func TestCompliance_DeploymentFilter(t *testing.T) {
	w := newReadWorld(t)
	commitVerifiableBackup(t, w, "db1", 0, []byte("a"))
	commitVerifiableBackup(t, w, "db2", 1, []byte("b"))

	stdout, _, exit := runCLI(t, "compliance", "report",
		"--repo", w.repoURL, "--deployment", "db1", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	var view complianceReportView
	bodyOf(t, stdout, &view)
	if view.DeploymentFilter != "db1" {
		t.Errorf("DeploymentFilter = %q", view.DeploymentFilter)
	}
	if view.Backups == nil || view.Backups.TotalCommitted != 1 {
		t.Errorf("Backups = %+v, want 1 (filtered)", view.Backups)
	}
}

// TestCompliance_SinceUntil_Bounds: explicit --since trims out an
// out-of-window backup.
func TestCompliance_SinceUntil_Bounds(t *testing.T) {
	w := newReadWorld(t)
	commitVerifiableBackup(t, w, "db1", 0, []byte("recent"))

	// 1-second window in the distant past — won't include the
	// backup whose StoppedAt is `now - hour + 0min`.
	stdout, _, exit := runCLI(t, "compliance", "report",
		"--repo", w.repoURL,
		"--since", "2020-01-01T00:00:00Z",
		"--until", "2020-01-01T00:00:01Z",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	var view complianceReportView
	bodyOf(t, stdout, &view)
	if view.Backups == nil || view.Backups.TotalCommitted != 0 {
		t.Errorf("Backups in distant-past window = %+v, want 0", view.Backups)
	}
}

// TestCompliance_MarkdownFormat: --format markdown returns the
// Markdown body when -o text. -o json still wins.
func TestCompliance_MarkdownFormat(t *testing.T) {
	w := newReadWorld(t)
	commitVerifiableBackup(t, w, "db1", 0, []byte("a"))

	stdout, _, exit := runCLI(t, "compliance", "report",
		"--repo", w.repoURL, "--format", "markdown", "-o", "text")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\n%s", exit, stdout)
	}
	for _, want := range []string{
		"# pg_hardstorage compliance report",
		"## Backup activity",
		"## Encryption coverage",
		"## KEK lifecycle",
		"## Audit chain",
		"db1",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("Markdown output missing %q:\n%s", want, stdout)
		}
	}

	// JSON output ignores --format markdown — still emits the v1
	// JSON body.
	stdout, _, exit = runCLI(t, "compliance", "report",
		"--repo", w.repoURL, "--format", "markdown", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	var any map[string]any
	if err := stdjson.Unmarshal([]byte(stdout), &any); err != nil {
		t.Errorf("--format markdown -o json should still emit valid JSON: %v\n%s", err, stdout)
	}
}

// TestCompliance_TextFormat_Compact: --format json -o text returns
// the compact human summary.
func TestCompliance_TextFormat_Compact(t *testing.T) {
	w := newReadWorld(t)
	commitVerifiableBackup(t, w, "db1", 0, []byte("a"))

	stdout, _, exit := runCLI(t, "compliance", "report",
		"--repo", w.repoURL, "-o", "text")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	for _, want := range []string{
		"compliance report",
		"Window:",
		"Backups committed:",
		"Encryption coverage:",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("text output missing %q:\n%s", want, stdout)
		}
	}
}

// TestCompliance_SkipFlags: every --no-* opt-out suppresses its
// section.
func TestCompliance_SkipFlags(t *testing.T) {
	w := newReadWorld(t)
	stdout, _, exit := runCLI(t, "compliance", "report",
		"--repo", w.repoURL,
		"--no-backups", "--no-encryption", "--no-verification",
		"--no-kek-lifecycle", "--no-approvals", "--no-holds",
		"--no-replicas", "--no-chain", "--no-worm",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	var view complianceReportView
	bodyOf(t, stdout, &view)
	if view.Backups != nil || view.Encryption != nil ||
		view.Verification != nil || view.KEKLifecycle != nil ||
		view.Approvals != nil || view.Holds != nil ||
		view.Replicas != nil || view.Chain != nil || view.WORM != nil {
		t.Errorf("expected all sections nil; got %+v", view)
	}
}

// TestCompliance_HelpDiscoverable: parent + subcommand help
// surface the right flags.
func TestCompliance_HelpDiscoverable(t *testing.T) {
	stdout, _, _ := runCLI(t, "compliance", "--help")
	if !strings.Contains(stdout, "report") {
		t.Errorf("compliance --help missing report subcommand:\n%s", stdout)
	}
	stdout, _, _ = runCLI(t, "compliance", "report", "--help")
	for _, want := range []string{
		"--since",
		"--until",
		"--format",
		"--deployment",
		"--no-backups",
		"--no-chain-verify",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("compliance report --help missing %q:\n%s", want, stdout)
		}
	}
}

// TestCompliance_PositionalURL: positional <url> works without --repo.
func TestCompliance_PositionalURL(t *testing.T) {
	w := newReadWorld(t)
	stdout, _, exit := runCLI(t, "compliance", "report", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\n%s", exit, stdout)
	}
	var view complianceReportView
	bodyOf(t, stdout, &view)
	if view.URL != w.repoURL {
		t.Errorf("URL = %q", view.URL)
	}
}
