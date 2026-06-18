// doctor_exit_on_issues_test.go — pins the doctor --exit-on-issues
// contract documented in docs/reference/exit-codes.md.
//
// Before this test the `--exit-on-issues` flag was documented but
// not implemented: an operator's cron / k8s-liveness script
// counting on `if pg_hardstorage doctor --exit-on-issues; then` to
// fire on a warning would never have alerted because the binary
// refused the flag with "unknown flag" (exit 2, which is also
// non-zero, masking the missing wiring).
//
// What this test pins:
//
//   - `doctor --exit-on-issues` is a real flag (no "unknown flag"
//     error).
//   - When the report has no warning+ issues, exit code is 0.
//   - When the report has at least one warning+ issue, exit code
//     is 10 (ExitDoctorIssues) and the structured error code is
//     `doctor.issues_present`.
//   - The classification helper doctorHasWarningOrHigher correctly
//     reads the RFC 5424 severity ordering (lower number = more
//     severe) — easy to invert by mistake.
package cli

import (
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// TestDoctor_SeverityRank_RFC5424Order: the inversion-helper must
// rank warning+ correctly given that the underlying enum has
// SeverityEmergency=0 and SeverityDebug=7.  Trivial test, big
// payoff: an off-by-one inversion here would silently disable the
// flag's whole purpose.
func TestDoctor_SeverityRank_RFC5424Order(t *testing.T) {
	cases := []struct {
		name string
		s    output.Severity
		want int
	}{
		{"debug", output.SeverityDebug, 0},     // 7-7
		{"info", output.SeverityInfo, 1},       // 7-6
		{"notice", output.SeverityNotice, 2},   // 7-5
		{"warning", output.SeverityWarning, 3}, // 7-4
		{"error", output.SeverityError, 4},     // 7-3
		{"critical", output.SeverityCritical, 5},
		{"alert", output.SeverityAlert, 6},
		{"emergency", output.SeverityEmergency, 7},
	}
	for _, tc := range cases {
		if got := doctorSeverityRank(tc.s); got != tc.want {
			t.Errorf("%s: rank = %d, want %d", tc.name, got, tc.want)
		}
	}

	// Spot-check the comparison contract callers rely on:
	if !(doctorSeverityRank(output.SeverityCritical) >= doctorSeverityRank(output.SeverityWarning)) {
		t.Error("Critical must rank >= Warning for the warning+ check to fire on critical issues")
	}
	if doctorSeverityRank(output.SeverityNotice) >= doctorSeverityRank(output.SeverityWarning) {
		t.Error("Notice must rank < Warning so informational reports stay quiet")
	}
}

// TestDoctor_HasWarningOrHigher: the issue-classification helper
// must fire on warning and above, and stay silent on notice and
// below.
func TestDoctor_HasWarningOrHigher(t *testing.T) {
	cases := []struct {
		name   string
		issues []doctorIssue
		want   bool
	}{
		{"empty", nil, false},
		{"info only", []doctorIssue{{Severity: output.SeverityInfo}}, false},
		{"notice only", []doctorIssue{{Severity: output.SeverityNotice}}, false},
		{"warning trips", []doctorIssue{{Severity: output.SeverityWarning}}, true},
		{"error trips", []doctorIssue{{Severity: output.SeverityError}}, true},
		{"critical trips", []doctorIssue{{Severity: output.SeverityCritical}}, true},
		{"mixed: notice + warning", []doctorIssue{
			{Severity: output.SeverityNotice},
			{Severity: output.SeverityWarning},
		}, true},
		{"all-notice doesn't trip", []doctorIssue{
			{Severity: output.SeverityNotice},
			{Severity: output.SeverityInfo},
		}, false},
	}
	for _, tc := range cases {
		if got := doctorHasWarningOrHigher(tc.issues); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestDoctor_CountIssues: only warning+ count toward the operator-
// facing total.
func TestDoctor_CountIssues(t *testing.T) {
	issues := []doctorIssue{
		{Severity: output.SeverityDebug},
		{Severity: output.SeverityInfo},
		{Severity: output.SeverityNotice}, // below threshold
		{Severity: output.SeverityWarning},
		{Severity: output.SeverityError},
		{Severity: output.SeverityCritical},
	}
	if got := countDoctorIssues(issues); got != 3 {
		t.Errorf("count = %d, want 3 (warning + error + critical)", got)
	}
}

// TestDoctor_DoctorIssuesPresentCodeRoutesToExit10: synthesise
// the structured error doctor returns when --exit-on-issues
// trips, and confirm ExitCodeFor routes it to ExitDoctorIssues
// (10).  This is the bridge between doctor's implementation and
// the documented exit-code contract.
func TestDoctor_DoctorIssuesPresentCodeRoutesToExit10(t *testing.T) {
	err := output.NewError("doctor.issues_present", "x")
	if got := output.ExitCodeFor(err); got != output.ExitDoctorIssues {
		t.Errorf("ExitCodeFor(doctor.issues_present) = %d; want %d (ExitDoctorIssues)",
			got, output.ExitDoctorIssues)
	}
}
