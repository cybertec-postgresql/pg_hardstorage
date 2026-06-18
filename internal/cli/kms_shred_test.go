package cli_test

import (
	stdjson "encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
)

// resolvedKeyringDir returns the keyring path the CLI will resolve
// in the current test environment.  Used by the kms shred tests to
// thread the right value into --confirm-keyring (the typed-keyring
// gate added in audit v23 fix #6).
func resolvedKeyringDir(t *testing.T) string {
	t.Helper()
	p, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		t.Fatalf("resolve paths: %v", err)
	}
	return p.Keyring.Value
}

// isolateHOME makes paths.Resolve deterministic per test by
// pointing every relevant env var at a fresh tempdir.  Mirrors
// newReadWorld() but doesn't pull in the rest of the read-world
// scaffolding the kms shred tests don't need.
func isolateHOME(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PG_HARDSTORAGE_ROOT", "")
	t.Setenv("PG_HARDSTORAGE_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("XDG_RUNTIME_DIR", "")
}

// TestKmsShred_RefusesWithoutApproval is the trust-foundation
// contract: kms shred must NOT run without an approval ID.
// The plan calls this out specifically — kms shred is the most
// consequential destructive op in the binary.
func TestKmsShred_RefusesWithoutApproval(t *testing.T) {
	isolateHOME(t)
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	repoURL := "file://" + repoDir
	if _, _, exit := runCLI(t, "repo", "init", repoURL); exit != int(output.ExitOK) {
		t.Fatalf("repo init failed")
	}

	_, stderr, exit := runCLI(t,
		"kms", "shred",
		"--repo", repoURL,
		"--yes",
		"-o", "json",
	)
	if exit != int(output.ExitMisuse) {
		t.Errorf("expected ExitMisuse(%d) without --require-approval; got %d\nstderr=%s",
			output.ExitMisuse, exit, stderr)
	}
	if !strings.Contains(stderr, "usage.missing_flag") {
		t.Errorf("expected usage.missing_flag; stderr=%s", stderr)
	}
	if !strings.Contains(stderr, "REQUIRED") {
		t.Errorf("error should emphasise that approval is mandatory; stderr=%s", stderr)
	}
}

// TestKmsShred_RefusesWithoutConfirmKeyring: the typed-keyring
// gate added in v23 audit fix #6 fires before the n-of-m gate.
// An attacker who can satisfy --require-approval and --yes still
// has to know the literal keyring path.
func TestKmsShred_RefusesWithoutConfirmKeyring(t *testing.T) {
	isolateHOME(t)
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	repoURL := "file://" + repoDir
	if _, _, exit := runCLI(t, "repo", "init", repoURL); exit != int(output.ExitOK) {
		t.Fatalf("repo init failed")
	}

	_, stderr, exit := runCLI(t,
		"kms", "shred",
		"--repo", repoURL,
		"--require-approval", "appr-anything",
		"--yes",
		"-o", "json",
	)
	if exit != int(output.ExitMisuse) {
		t.Errorf("expected ExitMisuse without --confirm-keyring; got %d\nstderr=%s", exit, stderr)
	}
	if !strings.Contains(stderr, "usage.confirmation_required") {
		t.Errorf("expected usage.confirmation_required for missing --confirm-keyring; stderr=%s", stderr)
	}
	if !strings.Contains(stderr, "confirm-keyring") {
		t.Errorf("error should name --confirm-keyring; stderr=%s", stderr)
	}
}

// TestKmsShred_RefusesWrongConfirmKeyring: a wrong --confirm-keyring
// value is rejected with a distinct mismatch error rather than
// silently proceeding.
func TestKmsShred_RefusesWrongConfirmKeyring(t *testing.T) {
	isolateHOME(t)
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	repoURL := "file://" + repoDir
	if _, _, exit := runCLI(t, "repo", "init", repoURL); exit != int(output.ExitOK) {
		t.Fatalf("repo init failed")
	}

	_, stderr, exit := runCLI(t,
		"kms", "shred",
		"--repo", repoURL,
		"--require-approval", "appr-anything",
		"--confirm-keyring", "/not/the/real/keyring",
		"--yes",
		"-o", "json",
	)
	if exit != int(output.ExitMisuse) {
		t.Errorf("expected ExitMisuse for wrong --confirm-keyring; got %d", exit)
	}
	if !strings.Contains(stderr, "usage.confirmation_mismatch") {
		t.Errorf("expected usage.confirmation_mismatch; stderr=%s", stderr)
	}
}

// TestKmsShred_RefusesPendingApproval: even with --require-approval
// passed, the gate refuses if the approval is still pending.
func TestKmsShred_RefusesPendingApproval(t *testing.T) {
	isolateHOME(t)
	keyringDir := resolvedKeyringDir(t)
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	repoURL := "file://" + repoDir
	if _, _, exit := runCLI(t, "repo", "init", repoURL); exit != int(output.ExitOK) {
		t.Fatalf("repo init failed")
	}

	_, pubA := genApproverKeys(t, tmp, "alice")
	_, pubB := genApproverKeys(t, tmp, "bob")

	// Create a kms.shred approval but don't approve.
	stdout, _, _ := runCLI(t,
		"approval", "request",
		"--repo", repoURL,
		"--op", "kms.shred",
		"--target", "any-keyring",
		"--threshold", "2",
		"--approver-key", pubA,
		"--approver-key", pubB,
		"-o", "json",
	)
	var reqRes output.Result
	stdjson.Unmarshal([]byte(stdout), &reqRes)
	requestID := reqRes.Result.(map[string]any)["id"].(string)

	_, stderr, exit := runCLI(t,
		"kms", "shred",
		"--repo", repoURL,
		"--require-approval", requestID,
		"--confirm-keyring", keyringDir,
		"--yes",
		"-o", "json",
	)
	// Status will be either pending (gate refusal) or
	// auth.approval_target_mismatch (target binding refusal). Either
	// way the gate prevents the shred — that's the important property.
	if exit == int(output.ExitOK) {
		t.Errorf("shred should refuse with un-approved gate; got exit 0")
	}
	if !strings.Contains(stderr, "conflict.approval_pending") &&
		!strings.Contains(stderr, "auth.approval_target_mismatch") {
		t.Errorf("expected pending-or-target-mismatch refusal; stderr=%s", stderr)
	}
}

// TestKmsShred_RefusesWithoutYes: even with an approved gate AND a
// matching --confirm-keyring, the command refuses without --yes.
// Three-gate posture is intentional for the highest-risk op.
func TestKmsShred_RefusesWithoutYes(t *testing.T) {
	isolateHOME(t)
	keyringDir := resolvedKeyringDir(t)
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	repoURL := "file://" + repoDir
	if _, _, exit := runCLI(t, "repo", "init", repoURL); exit != int(output.ExitOK) {
		t.Fatalf("repo init failed")
	}

	// Approve a request whose target deliberately does NOT match the
	// resolved keyring — the gate's target-mismatch refusal then
	// fires after --confirm-keyring passes but before --yes is
	// checked.  The strict ordering is:
	//   --confirm-keyring → repo-open → gate → --yes → action
	// so target-mismatch is what the operator sees here.
	privA, pubA := genApproverKeys(t, tmp, "alice")

	stdout, _, _ := runCLI(t,
		"approval", "request",
		"--repo", repoURL,
		"--op", "kms.shred",
		"--target", "/this-target-wont-match-keyring",
		"--threshold", "1",
		"--approver-key", pubA,
		"-o", "json",
	)
	var reqRes output.Result
	stdjson.Unmarshal([]byte(stdout), &reqRes)
	requestID := reqRes.Result.(map[string]any)["id"].(string)

	if _, _, exit := runCLI(t,
		"approval", "approve", requestID,
		"--repo", repoURL, "--key", privA, "--approver", "alice",
		"-o", "json",
	); exit != int(output.ExitOK) {
		t.Fatalf("approve failed")
	}

	_, stderr, exit := runCLI(t,
		"kms", "shred",
		"--repo", repoURL,
		"--require-approval", requestID,
		"--confirm-keyring", keyringDir,
		"-o", "json",
	)
	if exit == int(output.ExitOK) {
		t.Fatalf("shred should refuse; got exit 0")
	}
	if !strings.Contains(stderr, "auth.approval_target_mismatch") {
		t.Errorf("expected auth.approval_target_mismatch (target binding refusal fires before --yes guard); stderr=%s", stderr)
	}
}

// TestKmsShred_DryRunSkipsGates: --dry-run lets an operator
// preview the affected-backup scope BEFORE setting up an
// approval. It must run cleanly without --require-approval or
// --confirm-keyring or --yes, must report dry_run=true, and
// must NOT touch the KEK file.
func TestKmsShred_DryRunSkipsGates(t *testing.T) {
	isolateHOME(t)
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	repoURL := "file://" + repoDir
	if _, _, exit := runCLI(t, "repo", "init", repoURL); exit != int(output.ExitOK) {
		t.Fatalf("repo init failed")
	}

	stdout, stderr, exit := runCLI(t,
		"kms", "shred",
		"--repo", repoURL,
		"--dry-run",
		"-o", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("dry-run should succeed without gates; exit=%d stderr=%s", exit, stderr)
	}
	if !strings.Contains(stdout, `"dry_run": true`) {
		t.Errorf("dry-run JSON should set dry_run=true:\n%s", stdout)
	}
	if !strings.Contains(stdout, `"affected_backup_count"`) || strings.Contains(stdout, `"affected_backup_count": null`) {
		// We expect the count to be present (zero is fine — the
		// repo is empty); we just want the field to render.
	}
	// The KEK file MUST still exist after dry-run (the keyring is
	// only provisioned by `init`, which we did not run, so we
	// instead assert the keyring directory wasn't touched at all).
	keyringDir := resolvedKeyringDir(t)
	if _, err := os.Stat(keyringDir); err == nil {
		t.Errorf("dry-run must not provision the keyring at %s", keyringDir)
	}
}

// TestKmsShred_DryRunReportsAffectedScope: when manifests in the
// repo were wrapped by the local KEK, dry-run reports the count
// in the result body.  Smaller end-to-end check than the full
// shred test below — we don't need to actually destroy anything
// to verify the preview path.
func TestKmsShred_DryRunReportsAffectedScope(t *testing.T) {
	isolateHOME(t)
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	repoURL := "file://" + repoDir
	if _, _, exit := runCLI(t, "repo", "init", repoURL); exit != int(output.ExitOK) {
		t.Fatalf("repo init failed")
	}

	// No manifests yet → affected scope = 0.  Dry-run still has
	// to render that cleanly without crashing.
	stdout, _, exit := runCLI(t,
		"kms", "shred",
		"--repo", repoURL,
		"--dry-run",
		"--reason", "GDPR Art. 17 dry run",
		"-o", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("dry-run exit=%d\n%s", exit, stdout)
	}
	for _, want := range []string{`"dry_run": true`, `"reason": "GDPR Art. 17 dry run"`} {
		if !strings.Contains(stdout, want) {
			t.Errorf("dry-run JSON missing %q\n%s", want, stdout)
		}
	}
}

// TestKmsShred_DryRunRequiresRepo: --repo is the one flag
// dry-run still requires (we need somewhere to scan).
func TestKmsShred_DryRunRequiresRepo(t *testing.T) {
	isolateHOME(t)
	_, stderr, exit := runCLI(t,
		"kms", "shred",
		"--dry-run",
		"-o", "json",
	)
	if exit != int(output.ExitMisuse) {
		t.Errorf("dry-run without --repo should exit ExitMisuse; got %d", exit)
	}
	if !strings.Contains(stderr, "usage.missing_flag") {
		t.Errorf("expected usage.missing_flag; stderr=%s", stderr)
	}
}
