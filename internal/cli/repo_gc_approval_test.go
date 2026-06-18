package cli_test

import (
	stdjson "encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// TestRepoGC_RequireApproval_RefusesPending: the gate fires only on
// --apply and refuses with the conflict.approval_pending code when
// the approval is queued but not yet collected.
func TestRepoGC_RequireApproval_RefusesPending(t *testing.T) {
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

	stdout, stderr, exit := runCLI(t,
		"approval", "request",
		"--repo", repoURL,
		"--op", "repo.gc",
		"--target", repoURL,
		"--threshold", "2",
		"--approver-key", pubA,
		"--approver-key", pubB,
		"-o", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("request: %s\n%s", stdout, stderr)
	}
	var res output.Result
	stdjson.Unmarshal([]byte(stdout), &res)
	requestID := res.Result.(map[string]any)["id"].(string)

	// Pending — gate refuses.
	_, stderr, exit = runCLI(t,
		"repo", "gc", repoURL, "--apply",
		"--require-approval", requestID,
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

// TestRepoGC_RequireApproval_ApprovedFlow: full happy path — two
// approvers sign, --apply passes through the gate, audit chain
// records the action linked to the approval.
func TestRepoGC_RequireApproval_ApprovedFlow(t *testing.T) {
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

	stdout, _, _ := runCLI(t,
		"approval", "request",
		"--repo", repoURL,
		"--op", "repo.gc",
		"--target", repoURL,
		"--threshold", "2",
		"--approver-key", pubA,
		"--approver-key", pubB,
		"-o", "json",
	)
	var reqRes output.Result
	stdjson.Unmarshal([]byte(stdout), &reqRes)
	requestID := reqRes.Result.(map[string]any)["id"].(string)

	for _, k := range [][2]string{{privA, "alice"}, {privB, "bob"}} {
		if _, _, exit := runCLI(t,
			"approval", "approve", requestID,
			"--repo", repoURL, "--key", k[0], "--approver", k[1],
			"-o", "json",
		); exit != int(output.ExitOK) {
			t.Fatalf("approve %s failed", k[1])
		}
	}

	stdout, stderr, exit := runCLI(t,
		"repo", "gc", repoURL, "--apply",
		"--require-approval", requestID,
		"-o", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("gated apply: exit=%d\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}
	if !strings.Contains(stdout, requestID) {
		t.Errorf("expected approval_id in result body: %s", stdout)
	}

	// Audit chain should now hold a repo.gc event. The search
	// rendering is sparse (no body); we assert the action name +
	// non-zero count, which is the strongest claim that surface
	// supports.
	stdout, _, exit = runCLI(t,
		"audit", "search",
		"--repo", repoURL,
		"--action", "repo.gc",
		"-o", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("audit search: %s", stdout)
	}
	if !strings.Contains(stdout, `"action": "repo.gc"`) {
		t.Errorf("audit chain missing repo.gc event: %s", stdout)
	}
	if !strings.Contains(stdout, `"count": 1`) {
		t.Errorf("expected exactly one repo.gc event in audit chain: %s", stdout)
	}
}

// TestRepoGC_RequireApproval_OpMismatch: an approval for backup.delete
// must not be redeemable for repo.gc.
func TestRepoGC_RequireApproval_OpMismatch(t *testing.T) {
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

	// Approval for backup.delete (wrong op).
	stdout, _, _ := runCLI(t,
		"approval", "request",
		"--repo", repoURL,
		"--op", "backup.delete",
		"--target", "db1.full.x",
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
		"repo", "gc", repoURL, "--apply",
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

// TestRepoGC_RequireApproval_DryRunBypassesGate: dry-runs read only,
// so the gate shouldn't fire even with a stale or missing approval.
func TestRepoGC_RequireApproval_DryRunBypassesGate(t *testing.T) {
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	repoURL := "file://" + repoDir
	if _, _, exit := runCLI(t, "repo", "init", repoURL); exit != int(output.ExitOK) {
		t.Fatalf("repo init failed")
	}

	// Even with a totally bogus approval ID, dry-run should pass.
	stdout, stderr, exit := runCLI(t,
		"repo", "gc", repoURL,
		"--require-approval", "appr-doesnotexist",
		"-o", "json",
	)
	if exit != int(output.ExitOK) {
		t.Errorf("dry-run should bypass gate; got exit=%d\nstdout=%s\nstderr=%s",
			exit, stdout, stderr)
	}
	if !strings.Contains(stdout, `"dry_run": true`) {
		t.Errorf("expected dry_run=true in result: %s", stdout)
	}
}
