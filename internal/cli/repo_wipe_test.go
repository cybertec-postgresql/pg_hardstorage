package cli_test

import (
	stdjson "encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// TestRepoWipe_RefusesWithoutApprovalOrForce: an ordinary repo still
// refuses to wipe when given neither --require-approval nor --force —
// at least one authorisation path is mandatory (issue #57 added --force
// as the second path; it did not make wipe argument-free).
func TestRepoWipe_RefusesWithoutApprovalOrForce(t *testing.T) {
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
		"repo", "wipe", repoURL,
		"--yes",
		"-o", "json",
	)
	if exit != int(output.ExitMisuse) {
		t.Errorf("expected ExitMisuse(%d) without --require-approval/--force; got %d\nstderr=%s",
			output.ExitMisuse, exit, stderr)
	}
	if !strings.Contains(stderr, "usage.missing_flag") {
		t.Errorf("expected usage.missing_flag; stderr=%s", stderr)
	}
	// The error must point the operator at BOTH paths.
	if !strings.Contains(stderr, "--require-approval") || !strings.Contains(stderr, "--force") {
		t.Errorf("error should mention both --require-approval and --force; stderr=%s", stderr)
	}
}

// TestRepoWipe_ForceWipesNonWORM: issue #57 — a single operator can
// wipe an ordinary (non-WORM) repo with --force --yes, no approval.
func TestRepoWipe_ForceWipesNonWORM(t *testing.T) {
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
		"repo", "wipe", repoURL,
		"--force", "--yes",
		"--reason", "scratch repo",
		"-o", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("force wipe: exit=%d\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}
	if !strings.Contains(stdout, `"forced": true`) {
		t.Errorf("expected forced=true in result: %s", stdout)
	}
	if !strings.Contains(stdout, `"hsrepo_removed": true`) {
		t.Errorf("expected hsrepo_removed=true in result: %s", stdout)
	}
}

// TestRepoWipe_ForceStillNeedsYes: --force does not waive the
// irreversibility acknowledgement.
func TestRepoWipe_ForceStillNeedsYes(t *testing.T) {
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
		"repo", "wipe", repoURL,
		"--force", // no --yes
		"-o", "json",
	)
	if exit != int(output.ExitMisuse) {
		t.Errorf("expected ExitMisuse(%d) for --force without --yes; got %d\nstderr=%s",
			output.ExitMisuse, exit, stderr)
	}
	if !strings.Contains(stderr, "usage.confirmation_required") {
		t.Errorf("expected usage.confirmation_required; stderr=%s", stderr)
	}
}

// TestRepoWipe_RefusesPendingApproval: gate refuses if the
// approval is still pending.
func TestRepoWipe_RefusesPendingApproval(t *testing.T) {
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

	stdout, _, _ := runCLI(t,
		"approval", "request",
		"--repo", repoURL,
		"--op", "repo.wipe",
		"--target", repoURL,
		"--threshold", "2",
		"--approver-key", pubA,
		"--approver-key", pubB,
		"-o", "json",
	)
	var reqRes output.Result
	stdjson.Unmarshal([]byte(stdout), &reqRes)
	requestID := reqRes.Result.(map[string]any)["id"].(string)

	_, stderr, exit := runCLI(t,
		"repo", "wipe", repoURL,
		"--require-approval", requestID,
		"--yes",
		"-o", "json",
	)
	if exit != int(output.ExitConflict) {
		t.Errorf("expected ExitConflict(%d) on pending; got %d\nstderr=%s",
			output.ExitConflict, exit, stderr)
	}
	if !strings.Contains(stderr, "conflict.approval_pending") {
		t.Errorf("expected conflict.approval_pending; stderr=%s", stderr)
	}
}

// TestRepoWipe_HappyPath: full flow — request → approve →
// wipe --yes succeeds, repo is no longer openable.
func TestRepoWipe_HappyPath(t *testing.T) {
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	repoURL := "file://" + repoDir
	if _, _, exit := runCLI(t, "repo", "init", repoURL); exit != int(output.ExitOK) {
		t.Fatalf("repo init failed")
	}

	privA, pubA := genApproverKeys(t, tmp, "alice")

	// Approval bound to this repo URL.
	stdout, _, _ := runCLI(t,
		"approval", "request",
		"--repo", repoURL,
		"--op", "repo.wipe",
		"--target", repoURL,
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

	stdout, stderr, exit := runCLI(t,
		"repo", "wipe", repoURL,
		"--require-approval", requestID,
		"--yes",
		"--reason", "decommissioning tenant",
		"-o", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("wipe: exit=%d\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}
	if !strings.Contains(stdout, `"hsrepo_removed": true`) {
		t.Errorf("expected hsrepo_removed=true in result: %s", stdout)
	}
	if !strings.Contains(stdout, `"reason": "decommissioning tenant"`) {
		t.Errorf("expected reason in result: %s", stdout)
	}
}

// TestRepoWipe_RefusesWithoutYes: even with an approved gate, the
// command refuses without --yes. Two-gate posture.
func TestRepoWipe_RefusesWithoutYes(t *testing.T) {
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	repoURL := "file://" + repoDir
	if _, _, exit := runCLI(t, "repo", "init", repoURL); exit != int(output.ExitOK) {
		t.Fatalf("repo init failed")
	}

	privA, pubA := genApproverKeys(t, tmp, "alice")
	stdout, _, _ := runCLI(t,
		"approval", "request",
		"--repo", repoURL,
		"--op", "repo.wipe",
		"--target", repoURL,
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
		"repo", "wipe", repoURL,
		"--require-approval", requestID,
		// no --yes
		"-o", "json",
	)
	if exit != int(output.ExitMisuse) {
		t.Errorf("expected ExitMisuse(%d) without --yes; got %d\nstderr=%s",
			output.ExitMisuse, exit, stderr)
	}
	if !strings.Contains(stderr, "usage.confirmation_required") {
		t.Errorf("expected usage.confirmation_required; stderr=%s", stderr)
	}
}

// TestRepoWipe_OpMismatch: an approval for kms.shred must NOT
// redeem against repo.wipe.
func TestRepoWipe_OpMismatch(t *testing.T) {
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	repoURL := "file://" + repoDir
	if _, _, exit := runCLI(t, "repo", "init", repoURL); exit != int(output.ExitOK) {
		t.Fatalf("repo init failed")
	}

	privA, pubA := genApproverKeys(t, tmp, "alice")
	stdout, _, _ := runCLI(t,
		"approval", "request",
		"--repo", repoURL,
		"--op", "kms.shred", // wrong op
		"--target", repoURL,
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
		"repo", "wipe", repoURL,
		"--require-approval", requestID,
		"--yes",
		"-o", "json",
	)
	if exit != int(output.ExitAuth) {
		t.Errorf("expected ExitAuth(%d) for op-mismatch; got %d\nstderr=%s",
			output.ExitAuth, exit, stderr)
	}
	if !strings.Contains(stderr, "auth.approval_op_mismatch") {
		t.Errorf("expected auth.approval_op_mismatch; stderr=%s", stderr)
	}
}

// TestRepoWipe_AcceptsRepoFlag pins the surface-consistency fix: the repo
// URL may be supplied via --repo, identical to repo gc/audit/scrub, not only
// as the positional. The destructive gates are unchanged — this is a
// --force --yes wipe of a non-WORM repo, just addressed by --repo.
func TestRepoWipe_AcceptsRepoFlag(t *testing.T) {
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
		"repo", "wipe", "--repo", repoURL,
		"--force", "--yes",
		"-o", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("--repo-form force wipe: exit=%d\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}
	if !strings.Contains(stdout, `"hsrepo_removed": true`) {
		t.Errorf("expected the wipe to run via --repo: %s", stdout)
	}
}

// TestRepoWipe_RepoPositionalConflict: a positional URL that disagrees with
// --repo is a usage error, mirroring repo gc.
func TestRepoWipe_RepoPositionalConflict(t *testing.T) {
	_, stderr, exit := runCLI(t,
		"repo", "wipe", "file:///a", "--repo", "file:///b",
		"--force", "--yes", "-o", "json",
	)
	if exit != int(output.ExitMisuse) {
		t.Errorf("conflicting URLs should be ExitMisuse; got %d\nstderr=%s", exit, stderr)
	}
	if !strings.Contains(stderr, "usage.repo_conflict") {
		t.Errorf("expected usage.repo_conflict; stderr=%s", stderr)
	}
}

// TestRepoWipe_RequiresURLSomehow: neither positional nor --repo is a usage
// error (the URL is still mandatory — the fix added an alternative, it did
// not make wipe argument-free).
func TestRepoWipe_RequiresURLSomehow(t *testing.T) {
	_, stderr, exit := runCLI(t,
		"repo", "wipe", "--force", "--yes", "-o", "json",
	)
	if exit != int(output.ExitMisuse) {
		t.Errorf("no URL at all should be ExitMisuse; got %d\nstderr=%s", exit, stderr)
	}
	if !strings.Contains(stderr, "usage.missing_flag") {
		t.Errorf("expected usage.missing_flag; stderr=%s", stderr)
	}
}
