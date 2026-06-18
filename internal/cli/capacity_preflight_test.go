package cli_test

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// TestCapacityPreflight_Pass: explicit --projected-bytes
// against a fs:// repo with plenty of free space → pass.
func TestCapacityPreflight_Pass(t *testing.T) {
	w := newReadWorld(t)
	stdout, _, exit := runCLI(t, "capacity", "preflight",
		"--repo", w.repoURL,
		"--projected-bytes", "1024",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("preflight exit=%d\n%s", exit, stdout)
	}
	for _, want := range []string{
		`"verdict": "pass"`,
		`"projected_bytes": 1024`,
		`"available_bytes":`,
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q:\n%s", want, stdout)
		}
	}
}

// TestCapacityPreflight_InsufficientSpace: a projection
// larger than the disk's free space surfaces preflight.repo_full
// with ExitPreflight (4) per the v1 contract.
func TestCapacityPreflight_InsufficientSpace(t *testing.T) {
	w := newReadWorld(t)
	// 1 PiB — larger than any reasonable test machine's tmp
	// volume. The free-space probe will report less,
	// triggering the insufficient verdict.
	huge := int64(1 << 50)
	_, stderr, exit := runCLI(t, "capacity", "preflight",
		"--repo", w.repoURL,
		"--projected-bytes", strings.TrimSpace(formatInt64(huge)),
		"-o", "json")
	if exit != int(output.ExitPreflight) {
		t.Errorf("exit=%d, want ExitPreflight (%d)", exit, output.ExitPreflight)
	}
	for _, want := range []string{
		"preflight.repo_full",
		"available",
		"required",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing %q:\n%s", want, stderr)
		}
	}
}

// TestCapacityPreflight_FromDeployment_NoBackups_Refused:
// --from-deployment against a deployment with no committed
// backups refuses with notfound.backup + a Suggestion.
func TestCapacityPreflight_FromDeployment_NoBackups_Refused(t *testing.T) {
	w := newReadWorld(t)
	_, stderr, exit := runCLI(t, "capacity", "preflight",
		"--repo", w.repoURL,
		"--from-deployment", "db1",
		"-o", "json")
	if exit == int(output.ExitOK) {
		t.Fatalf("expected refusal; got OK")
	}
	if !strings.Contains(stderr, "notfound.backup") {
		t.Errorf("expected notfound.backup:\n%s", stderr)
	}
	if !strings.Contains(stderr, "no committed backups") {
		t.Errorf("expected 'no committed backups' message:\n%s", stderr)
	}
}

// TestCapacityPreflight_FromDeployment_HappyPath: with one
// committed backup, --from-deployment derives the projection
// from the manifest's logical bytes and passes.
func TestCapacityPreflight_FromDeployment_HappyPath(t *testing.T) {
	w := newReadWorld(t)
	commitVerifiableBackup(t, w, "db1", 0, []byte("dummy-bytes"))

	stdout, _, exit := runCLI(t, "capacity", "preflight",
		"--repo", w.repoURL,
		"--from-deployment", "db1",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("exit=%d\n%s", exit, stdout)
	}
	if !strings.Contains(stdout, `"verdict": "pass"`) {
		t.Errorf("expected verdict pass:\n%s", stdout)
	}
}

// TestCapacityPreflight_BothModes_RefusedAtUsage:
// --projected-bytes + --from-deployment both set → usage error.
func TestCapacityPreflight_BothModes_RefusedAtUsage(t *testing.T) {
	w := newReadWorld(t)
	_, stderr, exit := runCLI(t, "capacity", "preflight",
		"--repo", w.repoURL,
		"--projected-bytes", "1024",
		"--from-deployment", "db1",
		"-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit=%d, want ExitMisuse", exit)
	}
	if !strings.Contains(stderr, "usage.bad_flag") {
		t.Errorf("expected usage.bad_flag:\n%s", stderr)
	}
}

// TestCapacityPreflight_NeitherMode_RefusedAtUsage: missing
// both projection inputs → usage error.
func TestCapacityPreflight_NeitherMode_RefusedAtUsage(t *testing.T) {
	w := newReadWorld(t)
	_, stderr, exit := runCLI(t, "capacity", "preflight",
		"--repo", w.repoURL, "-o", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit=%d, want ExitMisuse", exit)
	}
	if !strings.Contains(stderr, "usage.missing_flag") {
		t.Errorf("expected usage.missing_flag:\n%s", stderr)
	}
}

// TestCapacityPreflight_HelpDiscoverable: capacity --help
// shows the new subcommand; its own --help advertises both
// projection modes.
func TestCapacityPreflight_HelpDiscoverable(t *testing.T) {
	stdout, _, _ := runCLI(t, "capacity", "--help")
	if !strings.Contains(stdout, "preflight") {
		t.Errorf("capacity --help missing preflight:\n%s", stdout)
	}
	stdout, _, _ = runCLI(t, "capacity", "preflight", "--help")
	for _, want := range []string{
		"--projected-bytes",
		"--from-deployment",
		"--safety-factor",
		"unsupported",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("preflight --help missing %q:\n%s", want, stdout)
		}
	}
}

// formatInt64 is a helper to keep the test's literal exit
// status intent readable without an "strconv" import that
// would clutter the test file.
func formatInt64(n int64) string {
	if n == 0 {
		return "0"
	}
	digits := make([]byte, 0, 20)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		digits = append(digits, byte('0'+n%10))
		n /= 10
	}
	if neg {
		digits = append(digits, '-')
	}
	for i, j := 0, len(digits)-1; i < j; i, j = i+1, j-1 {
		digits[i], digits[j] = digits[j], digits[i]
	}
	return string(digits)
}
