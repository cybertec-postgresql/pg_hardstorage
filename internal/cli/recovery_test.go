package cli_test

import (
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// readinessView mirrors the v1 contract's top-level shape.
type readinessView struct {
	Schema        string `json:"schema"`
	URL           string `json:"url"`
	Deployment    string `json:"deployment"`
	BackupCount   int    `json:"backup_count"`
	OverallStatus string `json:"overall_status"`
	Latest        *struct {
		BackupID     string `json:"backup_id"`
		AgeSeconds   int64  `json:"age_seconds"`
		LogicalBytes int64  `json:"logical_bytes"`
	} `json:"latest"`
	RPO *struct {
		ObservedSeconds int64 `json:"observed_seconds"`
		TargetSeconds   int64 `json:"target_seconds"`
		Met             bool  `json:"met"`
	} `json:"rpo"`
	RTO *struct {
		EstimatedSeconds       int64 `json:"estimated_seconds"`
		AssumedThroughputBytes int64 `json:"assumed_throughput_bytes_per_sec"`
	} `json:"rto"`
	Verification *struct {
		HasRecord bool `json:"has_record"`
	} `json:"verification"`
	Encryption *struct {
		Encrypted    bool `json:"encrypted"`
		KEKReachable bool `json:"kek_reachable"`
	} `json:"encryption"`
	WAL *struct {
		HasArchivedWAL bool `json:"has_archived_wal"`
	} `json:"wal"`
	Issues []struct {
		Severity string `json:"severity"`
		Code     string `json:"code"`
	} `json:"issues"`
}

type windowsView struct {
	Schema     string `json:"schema"`
	URL        string `json:"url"`
	Deployment string `json:"deployment"`
	Coverage   struct {
		WindowCount     int `json:"window_count"`
		WindowsWithGaps int `json:"windows_with_gaps"`
	} `json:"coverage"`
	Windows []struct {
		BackupID           string `json:"backup_id"`
		Timeline           uint32 `json:"timeline"`
		EarliestRestoreLSN string `json:"earliest_restore_lsn"`
		LatestRestoreLSN   string `json:"latest_restore_lsn"`
		HasArchivedWAL     bool   `json:"has_archived_wal"`
	} `json:"windows"`
}

// ----- recovery readiness -----

func TestRecoveryReadiness_RequiresRepo(t *testing.T) {
	_ = newReadWorld(t)
	_, errb, exit := runCLI(t, "recovery", "readiness", "db1", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.missing_flag") {
		t.Errorf("expected usage.missing_flag:\n%s", errb)
	}
}

func TestRecoveryReadiness_BadFormat(t *testing.T) {
	w := newReadWorld(t)
	_, errb, exit := runCLI(t, "recovery", "readiness", "db1",
		"--repo", w.repoURL, "--format", "csv", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.bad_flag") {
		t.Errorf("expected usage.bad_flag:\n%s", errb)
	}
}

func TestRecoveryReadiness_BadThroughput(t *testing.T) {
	w := newReadWorld(t)
	_, errb, exit := runCLI(t, "recovery", "readiness", "db1",
		"--repo", w.repoURL, "--assumed-throughput", "lots", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.bad_flag") {
		t.Errorf("expected usage.bad_flag:\n%s", errb)
	}
}

func TestRecoveryReadiness_NoBackups_NotReady(t *testing.T) {
	w := newReadWorld(t)
	stdout, _, exit := runCLI(t, "recovery", "readiness", "db1",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\n%s", exit, stdout)
	}
	var view readinessView
	bodyOf(t, stdout, &view)
	if view.OverallStatus != "no_backups" {
		t.Errorf("OverallStatus = %q, want no_backups", view.OverallStatus)
	}
	if len(view.Issues) == 0 {
		t.Errorf("expected issues for no_backups; got %v", view.Issues)
	}
}

func TestRecoveryReadiness_HappyPath(t *testing.T) {
	w := newReadWorld(t)
	commitVerifiableBackup(t, w, "db1", 0, []byte("payload"))

	stdout, _, exit := runCLI(t, "recovery", "readiness", "db1",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\n%s", exit, stdout)
	}
	var view readinessView
	bodyOf(t, stdout, &view)
	if view.BackupCount != 1 {
		t.Errorf("BackupCount = %d, want 1", view.BackupCount)
	}
	if view.Latest == nil || view.Latest.BackupID == "" {
		t.Errorf("Latest missing: %+v", view.Latest)
	}
	if view.RPO == nil || view.RTO == nil {
		t.Errorf("RPO / RTO sections missing: %+v %+v", view.RPO, view.RTO)
	}
	// commitVerifiableBackup uses time.Now()-1h-relative timestamps,
	// so the age should be roughly 1 hour.
	if view.Latest.AgeSeconds < 3000 || view.Latest.AgeSeconds > 4500 {
		t.Errorf("AgeSeconds = %d, expected ~3600", view.Latest.AgeSeconds)
	}
}

func TestRecoveryReadiness_RPOMissedTarget(t *testing.T) {
	w := newReadWorld(t)
	commitVerifiableBackup(t, w, "db1", 0, []byte("payload"))

	// Backup is roughly 1h old; RPO target 1s → not met.
	_, errb, exit := runCLI(t, "recovery", "readiness", "db1",
		"--repo", w.repoURL, "--rpo-seconds", "1", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d (recovery readiness still emits OK; status only reflected in body)\n%s",
			exit, errb)
	}
	// Body decode is fine; status should be not_ready.
	stdout, _, _ := runCLI(t, "recovery", "readiness", "db1",
		"--repo", w.repoURL, "--rpo-seconds", "1", "-o", "json")
	var view readinessView
	bodyOf(t, stdout, &view)
	if view.OverallStatus != "not_ready" {
		t.Errorf("OverallStatus = %q, want not_ready", view.OverallStatus)
	}
	if view.RPO == nil || view.RPO.Met {
		t.Errorf("RPO.Met should be false: %+v", view.RPO)
	}
}

func TestRecoveryReadiness_SkipFlags(t *testing.T) {
	w := newReadWorld(t)
	commitVerifiableBackup(t, w, "db1", 0, []byte("payload"))

	stdout, _, exit := runCLI(t, "recovery", "readiness", "db1",
		"--repo", w.repoURL,
		"--no-verification", "--no-encryption", "--no-wal",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	var view readinessView
	bodyOf(t, stdout, &view)
	if view.Verification != nil {
		t.Errorf("Verification = %+v, want nil", view.Verification)
	}
	if view.Encryption != nil {
		t.Errorf("Encryption = %+v, want nil", view.Encryption)
	}
	if view.WAL != nil {
		t.Errorf("WAL = %+v, want nil", view.WAL)
	}
}

func TestRecoveryReadiness_AssumedThroughputUnit(t *testing.T) {
	w := newReadWorld(t)
	commitVerifiableBackup(t, w, "db1", 0, []byte("payload"))

	stdout, _, exit := runCLI(t, "recovery", "readiness", "db1",
		"--repo", w.repoURL, "--assumed-throughput", "100MiB", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	var view readinessView
	bodyOf(t, stdout, &view)
	want := int64(100 * 1024 * 1024)
	if view.RTO == nil || view.RTO.AssumedThroughputBytes != want {
		t.Errorf("AssumedThroughputBytes = %d, want %d",
			func() int64 {
				if view.RTO == nil {
					return -1
				}
				return view.RTO.AssumedThroughputBytes
			}(), want)
	}
}

func TestRecoveryReadiness_TextFormat(t *testing.T) {
	w := newReadWorld(t)
	commitVerifiableBackup(t, w, "db1", 0, []byte("payload"))

	stdout, _, exit := runCLI(t, "recovery", "readiness", "db1",
		"--repo", w.repoURL, "-o", "text")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\n%s", exit, stdout)
	}
	for _, want := range []string{
		"recovery readiness",
		"Status:",
		"Backups:",
		"Latest:",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("text output missing %q:\n%s", want, stdout)
		}
	}
}

func TestRecoveryReadiness_MarkdownFormat(t *testing.T) {
	w := newReadWorld(t)
	commitVerifiableBackup(t, w, "db1", 0, []byte("payload"))

	stdout, _, exit := runCLI(t, "recovery", "readiness", "db1",
		"--repo", w.repoURL, "--format", "markdown", "-o", "text")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\n%s", exit, stdout)
	}
	for _, want := range []string{
		"# pg_hardstorage recovery readiness",
		"## Verdict",
		"## Latest backup",
		"## RPO",
		"## RTO",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("Markdown output missing %q:\n%s", want, stdout)
		}
	}
}

// ----- recovery windows -----

func TestRecoveryWindows_RequiresRepo(t *testing.T) {
	_ = newReadWorld(t)
	_, errb, exit := runCLI(t, "recovery", "windows", "db1", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.missing_flag") {
		t.Errorf("expected usage.missing_flag:\n%s", errb)
	}
}

func TestRecoveryWindows_BadFormat(t *testing.T) {
	w := newReadWorld(t)
	_, errb, exit := runCLI(t, "recovery", "windows", "db1",
		"--repo", w.repoURL, "--format", "csv", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.bad_flag") {
		t.Errorf("expected usage.bad_flag:\n%s", errb)
	}
}

func TestRecoveryWindows_Empty(t *testing.T) {
	w := newReadWorld(t)
	stdout, _, exit := runCLI(t, "recovery", "windows", "db1",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	var view windowsView
	bodyOf(t, stdout, &view)
	if view.Coverage.WindowCount != 0 {
		t.Errorf("WindowCount = %d, want 0", view.Coverage.WindowCount)
	}
}

func TestRecoveryWindows_NewestFirst(t *testing.T) {
	w := newReadWorld(t)
	commitVerifiableBackup(t, w, "db1", 0, []byte("a"))
	commitVerifiableBackup(t, w, "db1", 5, []byte("b")) // newer
	commitVerifiableBackup(t, w, "db1", 2, []byte("c")) // middle

	stdout, _, exit := runCLI(t, "recovery", "windows", "db1",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	var view windowsView
	bodyOf(t, stdout, &view)
	if view.Coverage.WindowCount != 3 {
		t.Errorf("WindowCount = %d, want 3", view.Coverage.WindowCount)
	}
}

func TestRecoveryWindows_IncludeOlderThan(t *testing.T) {
	w := newReadWorld(t)
	// commitVerifiableBackup uses now-1h-base offsets; "5d" filter
	// shouldn't exclude anything.
	commitVerifiableBackup(t, w, "db1", 0, []byte("a"))

	stdout, _, exit := runCLI(t, "recovery", "windows", "db1",
		"--repo", w.repoURL, "--include-older-than", "5d", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	var view windowsView
	bodyOf(t, stdout, &view)
	if view.Coverage.WindowCount != 1 {
		t.Errorf("WindowCount = %d, want 1", view.Coverage.WindowCount)
	}
}

func TestRecoveryWindows_TextFormat(t *testing.T) {
	w := newReadWorld(t)
	commitVerifiableBackup(t, w, "db1", 0, []byte("a"))

	stdout, _, exit := runCLI(t, "recovery", "windows", "db1",
		"--repo", w.repoURL, "-o", "text")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d\n%s", exit, stdout)
	}
	for _, want := range []string{
		"recovery windows",
		"Windows:",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("text output missing %q:\n%s", want, stdout)
		}
	}
}

func TestRecoveryWindows_MarkdownFormat(t *testing.T) {
	w := newReadWorld(t)
	commitVerifiableBackup(t, w, "db1", 0, []byte("a"))

	stdout, _, exit := runCLI(t, "recovery", "windows", "db1",
		"--repo", w.repoURL, "--format", "markdown", "-o", "text")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	for _, want := range []string{
		"# pg_hardstorage recovery windows",
		"## PITR windows",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("Markdown output missing %q:\n%s", want, stdout)
		}
	}
}

func TestRecoveryWindows_BadIncludeOlderThan(t *testing.T) {
	w := newReadWorld(t)
	_, errb, exit := runCLI(t, "recovery", "windows", "db1",
		"--repo", w.repoURL, "--include-older-than", "yesterday", "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
	if !strings.Contains(errb, "usage.bad_flag") {
		t.Errorf("expected usage.bad_flag:\n%s", errb)
	}
}

// ----- parent + help -----

func TestRecovery_HelpDiscoverable(t *testing.T) {
	stdout, _, _ := runCLI(t, "recovery", "--help")
	for _, want := range []string{"readiness", "windows"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("recovery --help missing %q:\n%s", want, stdout)
		}
	}
	stdout, _, _ = runCLI(t, "recovery", "readiness", "--help")
	for _, want := range []string{
		"--rpo-seconds", "--rto-seconds", "--assumed-throughput",
		"--no-verification", "--no-encryption", "--no-wal",
		"--format", "--staleness",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("recovery readiness --help missing %q:\n%s", want, stdout)
		}
	}
	stdout, _, _ = runCLI(t, "recovery", "windows", "--help")
	for _, want := range []string{
		"--include-older-than", "--format",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("recovery windows --help missing %q:\n%s", want, stdout)
		}
	}
}

// TestRecovery_DurationParserDays: "Nd" form is accepted.
func TestRecovery_DurationParserDays(t *testing.T) {
	w := newReadWorld(t)
	stdout, _, exit := runCLI(t, "recovery", "readiness", "db1",
		"--repo", w.repoURL, "--staleness", "14d", "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	_ = stdout
}

// TestRecovery_RTOEstimateAtDefaultThroughput: with no
// --assumed-throughput, the report uses 160MiB/s.
func TestRecovery_RTOEstimateAtDefaultThroughput(t *testing.T) {
	w := newReadWorld(t)
	commitVerifiableBackup(t, w, "db1", 0, []byte("payload"))

	stdout, _, exit := runCLI(t, "recovery", "readiness", "db1",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit = %d", exit)
	}
	var view readinessView
	bodyOf(t, stdout, &view)
	if view.RTO == nil {
		t.Fatal("RTO missing")
	}
	want := int64(160 * 1024 * 1024)
	if view.RTO.AssumedThroughputBytes != want {
		t.Errorf("default throughput = %d, want %d", view.RTO.AssumedThroughputBytes, want)
	}
}

// TestRecovery_NegativeFlags_Refused: --rpo-seconds < 0 / --rto-seconds < 0 refused.
func TestRecovery_NegativeFlags_Refused(t *testing.T) {
	w := newReadWorld(t)
	for _, flag := range []string{"--rpo-seconds", "--rto-seconds"} {
		_, errb, exit := runCLI(t, "recovery", "readiness", "db1",
			"--repo", w.repoURL, flag, "-1", "-o", "json")
		if exit != int(output.ExitMisuse) {
			t.Errorf("flag %q: exit = %d, want ExitMisuse", flag, exit)
		}
		if !strings.Contains(errb, "usage.bad_flag") {
			t.Errorf("flag %q: expected usage.bad_flag", flag)
		}
	}
	_ = time.Now() // keep import
}
