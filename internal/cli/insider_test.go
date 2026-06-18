package cli_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/insider"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

type insiderScanView struct {
	ID             string    `json:"id"`
	StartedAt      time.Time `json:"started_at"`
	Tenant         string    `json:"tenant,omitempty"`
	BaselineEvents int       `json:"baseline_events"`
	TargetEvents   int       `json:"target_events"`
	Findings       []struct {
		Type     string `json:"type"`
		Severity string `json:"severity"`
		Actor    string `json:"actor"`
		Action   string `json:"action,omitempty"`
		Reason   string `json:"reason"`
	} `json:"findings,omitempty"`
}

type insiderListView struct {
	Count   int `json:"count"`
	Entries []struct {
		ID              string    `json:"id"`
		StartedAt       time.Time `json:"started_at"`
		Findings        int       `json:"findings"`
		HighestSeverity string    `json:"highest_severity,omitempty"`
	} `json:"entries"`
}

// plantInsiderEvent appends a single audit event with explicit
// timestamp/actor/tenant/action.  Mirrors the helper from
// insider_test.go so the CLI tests don't reach into internal.
func plantInsiderEvent(t *testing.T, w *readWorld, at time.Time, actor, tenant, action string) {
	t.Helper()
	ev := &audit.Event{
		Action:    action,
		Actor:     actor,
		Tenant:    tenant,
		Subject:   audit.Subject{Tenant: tenant},
		Timestamp: at,
	}
	store := audit.NewStore(w.sp)
	if err := store.Append(context.Background(), ev); err != nil {
		t.Fatalf("Append: %v", err)
	}
}

// ----- scan -----

func TestInsiderScan_RequiresRepo(t *testing.T) {
	_ = newReadWorld(t)
	_, errb, exit := runCLI(t, "insider", "scan", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.missing_flag") {
		t.Errorf("expected usage.missing_flag:\n%s", errb)
	}
}

func TestInsiderScan_BadFailOn(t *testing.T) {
	w := newReadWorld(t)
	_, errb, exit := runCLI(t, "insider", "scan",
		"--repo", w.repoURL, "--fail-on", "exotic", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.bad_flag") {
		t.Errorf("expected usage.bad_flag:\n%s", errb)
	}
}

func TestInsiderScan_NoFindings(t *testing.T) {
	w := newReadWorld(t)
	stdout, _, exit := runCLI(t, "insider", "scan",
		"--repo", w.repoURL,
		"--baseline", "720h", "--target", "24h",
		"--note", "first run",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\n%s", exit, stdout)
	}
	var v insiderScanView
	bodyOf(t, stdout, &v)
	if len(v.Findings) != 0 {
		t.Errorf("expected no findings, got %d", len(v.Findings))
	}
}

func TestInsiderScan_DetectsCriticalAndExits9(t *testing.T) {
	w := newReadWorld(t)
	now := time.Now().UTC()
	// Baseline: alice does only reads.
	for i := 0; i < 5; i++ {
		plantInsiderEvent(t, w,
			now.Add(-time.Duration(i+1)*48*time.Hour),
			"alice@acme", "default", "backup.read")
	}
	// Target: alice runs kms.shred (FirstDestructive → critical).
	plantInsiderEvent(t, w, now.Add(-30*time.Minute),
		"alice@acme", "default", "kms.shred")

	stdout, errb, exit := runCLI(t, "insider", "scan",
		"--repo", w.repoURL,
		"--fail-on", "critical",
		"-o", "json")
	if exit != int(output.ExitVerifyFailed) {
		t.Fatalf("exit = %d, want ExitVerifyFailed (9)\n%s", exit, errb)
	}
	if !strings.Contains(errb, "verify.insider_findings") {
		t.Errorf("expected verify.insider_findings:\n%s", errb)
	}
	// Dual-stream: body still on stdout.
	var v insiderScanView
	bodyOf(t, stdout, &v)
	hasCritical := false
	for _, f := range v.Findings {
		if f.Severity == "critical" {
			hasCritical = true
			break
		}
	}
	if !hasCritical {
		t.Errorf("body has no critical finding: %+v", v.Findings)
	}
}

func TestInsiderScan_FailOnNoneNeverFails(t *testing.T) {
	w := newReadWorld(t)
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		plantInsiderEvent(t, w,
			now.Add(-time.Duration(i+1)*48*time.Hour),
			"alice@acme", "default", "backup.read")
	}
	plantInsiderEvent(t, w, now.Add(-30*time.Minute),
		"alice@acme", "default", "kms.shred")

	_, _, exit := runCLI(t, "insider", "scan",
		"--repo", w.repoURL,
		"--fail-on", "none",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Errorf("exit = %d, want ExitOK with --fail-on none", exit)
	}
}

func TestInsiderScan_PersistsScan(t *testing.T) {
	w := newReadWorld(t)
	stdout, _, exit := runCLI(t, "insider", "scan",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	var scan insiderScanView
	bodyOf(t, stdout, &scan)

	stdout, _, exit = runCLI(t, "insider", "list",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("list exit = %d", exit)
	}
	var list insiderListView
	bodyOf(t, stdout, &list)
	if list.Count != 1 {
		t.Errorf("Count = %d, want 1", list.Count)
	}
	if list.Entries[0].ID != scan.ID {
		t.Errorf("ID drift: list %q vs scan %q",
			list.Entries[0].ID, scan.ID)
	}
}

// ----- list -----

func TestInsiderList_RequiresRepo(t *testing.T) {
	_ = newReadWorld(t)
	_, errb, exit := runCLI(t, "insider", "list", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.missing_flag") {
		t.Errorf("expected usage.missing_flag:\n%s", errb)
	}
}

func TestInsiderList_BadSeverity(t *testing.T) {
	w := newReadWorld(t)
	_, errb, exit := runCLI(t, "insider", "list",
		"--repo", w.repoURL, "--min-severity", "exotic", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.bad_flag") {
		t.Errorf("expected usage.bad_flag:\n%s", errb)
	}
}

func TestInsiderList_BadSince(t *testing.T) {
	w := newReadWorld(t)
	_, errb, exit := runCLI(t, "insider", "list",
		"--repo", w.repoURL, "--since", "yesterday", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.bad_flag") {
		t.Errorf("expected usage.bad_flag:\n%s", errb)
	}
}

func TestInsiderList_Empty(t *testing.T) {
	w := newReadWorld(t)
	stdout, _, exit := runCLI(t, "insider", "list",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	var v insiderListView
	bodyOf(t, stdout, &v)
	if v.Count != 0 {
		t.Errorf("Count = %d", v.Count)
	}
}

func TestInsiderList_WithFindingsFilter(t *testing.T) {
	w := newReadWorld(t)
	now := time.Now().UTC()
	// Plant data that triggers a finding.
	for i := 0; i < 5; i++ {
		plantInsiderEvent(t, w,
			now.Add(-time.Duration(i+1)*48*time.Hour),
			"alice@acme", "default", "backup.read")
	}
	plantInsiderEvent(t, w, now.Add(-30*time.Minute),
		"alice@acme", "default", "kms.shred")

	// First scan finds the issue (critical).  Use --fail-on none so
	// scan exits 0 even with finding.
	if _, _, exit := runCLI(t, "insider", "scan",
		"--repo", w.repoURL, "--fail-on", "none", "-o", "json"); exit != int(output.ExitOK) {
		t.Fatalf("scan exit = %d", exit)
	}
	// Wait a second, run a clean scan.
	time.Sleep(time.Second)
	// Restart the audit log isn't possible, so we just produce
	// another scan against the same audit history; it WILL still
	// produce a finding.  Instead, test --with-findings against
	// the single existing scan (filter is a no-op when the scan
	// has findings).
	stdout, _, exit := runCLI(t, "insider", "list",
		"--repo", w.repoURL,
		"--with-findings", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("list exit = %d", exit)
	}
	var list insiderListView
	bodyOf(t, stdout, &list)
	if list.Count != 1 {
		t.Errorf("with-findings list count = %d, want 1", list.Count)
	}
	if list.Entries[0].HighestSeverity == "" {
		t.Errorf("HighestSeverity empty")
	}
}

// ----- show -----

func TestInsiderShow_NotFound(t *testing.T) {
	w := newReadWorld(t)
	_, errb, exit := runCLI(t, "insider", "show", "ghost",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitNotFound) {
		t.Errorf("exit = %d, want ExitNotFound", exit)
	}
	if !strings.Contains(errb, "notfound.scan") {
		t.Errorf("expected notfound.scan:\n%s", errb)
	}
}

func TestInsiderShow_Happy(t *testing.T) {
	w := newReadWorld(t)
	// Plant a finding-producing event.
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		plantInsiderEvent(t, w,
			now.Add(-time.Duration(i+1)*48*time.Hour),
			"alice@acme", "default", "backup.read")
	}
	plantInsiderEvent(t, w, now.Add(-30*time.Minute),
		"bob@acme", "default", "backup.read")

	stdout, _, exit := runCLI(t, "insider", "scan",
		"--repo", w.repoURL, "--fail-on", "none", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("scan exit = %d", exit)
	}
	var scan insiderScanView
	bodyOf(t, stdout, &scan)

	stdout, _, exit = runCLI(t, "insider", "show", scan.ID,
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("show exit = %d", exit)
	}
	var got insiderScanView
	bodyOf(t, stdout, &got)
	if got.ID != scan.ID {
		t.Errorf("ID drift: %q vs %q", got.ID, scan.ID)
	}
	hasNovel := false
	for _, f := range got.Findings {
		if f.Type == string(insider.FindingNovelPrincipal) && f.Actor == "bob@acme" {
			hasNovel = true
		}
	}
	if !hasNovel {
		t.Errorf("expected novel-principal for bob@acme: %+v", got.Findings)
	}
}

// TestInsider_HelpDiscoverable: parent help names every subcommand.
func TestInsider_HelpDiscoverable(t *testing.T) {
	stdout, _, exit := runCLI(t, "insider", "--help")
	if exit != int(output.ExitOK) {
		t.Fatalf("help exit = %d\n%s", exit, stdout)
	}
	for _, want := range []string{"scan", "list", "show"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("help missing %q:\n%s", want, stdout)
		}
	}
}
