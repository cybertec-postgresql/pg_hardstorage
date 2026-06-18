package cli_test

import (
	stdjson "encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// TestRepoSetMode_RequireApproval_ApprovedFlow exercises the
// happy-path gate: create approval, two operators approve, set-mode
// flips through the gate. This is the e2e demo of the n-of-m
// workflow protecting a real destructive op.
func TestRepoSetMode_RequireApproval_ApprovedFlow(t *testing.T) {
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
	privB, pubB := genApproverKeys(t, tmp, "bob")

	// Create an approval request bound to repo.set_mode + this URL.
	stdout, stderr, exit := runCLI(t,
		"approval", "request",
		"--repo", repoURL,
		"--op", "repo.set_mode",
		"--target", repoURL,
		"--reason", "incident response: lock writes",
		"--threshold", "2",
		"--approver-key", pubA,
		"--approver-key", pubB,
		"-o", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("approval request: exit=%d\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}
	var reqRes output.Result
	stdjson.Unmarshal([]byte(stdout), &reqRes)
	requestID := reqRes.Result.(map[string]any)["id"].(string)

	// Pre-approval: set-mode --require-approval must refuse with
	// "still pending" code.
	_, stderr, exit = runCLI(t,
		"repo", "set-mode", repoURL, "read-only",
		"--require-approval", requestID,
		"-o", "json",
	)
	if exit != int(output.ExitConflict) {
		t.Errorf("expected ExitConflict(%d) for pending approval; got %d\nstderr=%s",
			output.ExitConflict, exit, stderr)
	}
	if !strings.Contains(stderr, "conflict.approval_pending") {
		t.Errorf("expected conflict.approval_pending; stderr=%s", stderr)
	}

	// Both approvers sign.
	for _, k := range [][2]string{{privA, "alice@acme"}, {privB, "bob@acme"}} {
		_, stderr, exit = runCLI(t,
			"approval", "approve", requestID,
			"--repo", repoURL,
			"--key", k[0],
			"--approver", k[1],
			"-o", "json",
		)
		if exit != int(output.ExitOK) {
			t.Fatalf("approve %s: exit=%d stderr=%s", k[1], exit, stderr)
		}
	}

	// Now the gated set-mode should pass.
	stdout, stderr, exit = runCLI(t,
		"repo", "set-mode", repoURL, "read-only",
		"--require-approval", requestID,
		"-o", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("approved set-mode: exit=%d\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}
	if !strings.Contains(stdout, `"mode": "read-only"`) {
		t.Errorf("expected mode read-only in result: %s", stdout)
	}
	if !strings.Contains(stdout, requestID) {
		t.Errorf("expected approval_id in result: %s", stdout)
	}
}

// TestRepoSetMode_RequireApproval_OpMismatch is the trust-foundation
// CLI assertion: an approval for backup.delete cannot be redeemed
// against repo.set_mode.
func TestRepoSetMode_RequireApproval_OpMismatch(t *testing.T) {
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

	// Approval for the WRONG op.
	stdout, stderr, exit := runCLI(t,
		"approval", "request",
		"--repo", repoURL,
		"--op", "backup.delete",
		"--target", "db1.full.x",
		"--threshold", "1",
		"--approver-key", pubA,
		"-o", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("request: %s\n%s", stdout, stderr)
	}
	var res output.Result
	stdjson.Unmarshal([]byte(stdout), &res)
	requestID := res.Result.(map[string]any)["id"].(string)

	// Get it approved.
	if _, _, exit := runCLI(t,
		"approval", "approve", requestID,
		"--repo", repoURL, "--key", privA, "--approver", "alice",
		"-o", "json",
	); exit != int(output.ExitOK) {
		t.Fatalf("approve failed")
	}

	// Try to redeem the backup.delete approval against
	// repo.set_mode. The gate must refuse with a structured
	// auth.approval_op_mismatch error.
	_, stderr, exit = runCLI(t,
		"repo", "set-mode", repoURL, "read-only",
		"--require-approval", requestID,
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

// TestRepoSetMode_RequireApproval_TargetMismatch — symmetric guard:
// approval for repo URL X cannot be redeemed against repo URL Y.
func TestRepoSetMode_RequireApproval_TargetMismatch(t *testing.T) {
	tmp := t.TempDir()
	repoA := filepath.Join(tmp, "repoA")
	repoB := filepath.Join(tmp, "repoB")
	for _, p := range []string{repoA, repoB} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	urlA := "file://" + repoA
	urlB := "file://" + repoB
	for _, u := range []string{urlA, urlB} {
		if _, _, exit := runCLI(t, "repo", "init", u); exit != int(output.ExitOK) {
			t.Fatalf("init %s", u)
		}
	}

	privA, pubA := genApproverKeys(t, tmp, "alice")

	// Approval bound to urlA.
	stdout, _, _ := runCLI(t,
		"approval", "request",
		"--repo", urlA,
		"--op", "repo.set_mode",
		"--target", urlA,
		"--threshold", "1",
		"--approver-key", pubA,
		"-o", "json",
	)
	var res output.Result
	stdjson.Unmarshal([]byte(stdout), &res)
	requestID := res.Result.(map[string]any)["id"].(string)

	if _, _, exit := runCLI(t,
		"approval", "approve", requestID,
		"--repo", urlA, "--key", privA, "--approver", "alice",
		"-o", "json",
	); exit != int(output.ExitOK) {
		t.Fatal("approve failed")
	}

	// The approval lives in urlA; our `set-mode urlA` happy-path
	// works (verified in another test). What we test here: the gate
	// won't redeem an approval whose Target is urlA against an
	// attempted set-mode on urlB. We need the approval to be
	// readable from urlB's repo to even reach the target check —
	// since approvals are repo-scoped, attempting to redeem from
	// urlB hits notfound.approval first. The cleaner trust-binding
	// test is in the package-level Gate test (TestGate_Refuses
	// TargetMismatch) which exercises the exact byte comparison.
	//
	// Here we assert the CLI surfaces notfound.approval (which is
	// itself a refusal — the approval doesn't exist in urlB's
	// approvals/ prefix).
	_, stderr, exit := runCLI(t,
		"repo", "set-mode", urlB, "read-only",
		"--require-approval", requestID,
		"-o", "json",
	)
	if exit == int(output.ExitOK) {
		t.Errorf("cross-repo redemption should fail; got exit 0\nstderr=%s", stderr)
	}
	if !strings.Contains(stderr, "notfound.approval") {
		t.Errorf("expected notfound.approval (approval lives in urlA, not urlB); stderr=%s", stderr)
	}
}
