package cli_test

import (
	stdjson "encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// TestStatus_FreshRepoEmptyChainHealthy: fresh repo + no audit
// events + no approvals → status reports anchor "fresh" (chain
// empty is healthy) and 0 pending approvals.
func TestStatus_FreshRepoEmptyChainHealthy(t *testing.T) {
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
		"status",
		"--repo", repoURL,
		"-o", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("status: exit=%d\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}
	var res output.Result
	if err := stdjson.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout)
	}
	body, _ := stdjson.Marshal(res.Result)
	bodyStr := string(body)
	if !strings.Contains(bodyStr, `"audit_anchor":`) {
		t.Errorf("body missing audit_anchor: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, `"fresh":true`) {
		t.Errorf("fresh empty repo should report anchor.fresh=true: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, `"pending_approvals":0`) {
		t.Errorf("pending_approvals should be 0 on fresh repo: %s", bodyStr)
	}
}

// TestStatus_StaleAnchor: chain has events but no anchor → status
// reports anchor.fresh=false and surfaces the un-anchored count.
func TestStatus_StaleAnchor(t *testing.T) {
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	repoURL := "file://" + repoDir
	if _, _, exit := runCLI(t, "repo", "init", repoURL); exit != int(output.ExitOK) {
		t.Fatalf("repo init failed")
	}
	if _, _, exit := runCLI(t,
		"audit", "append", "operator.test",
		"--repo", repoURL,
		"-o", "json",
	); exit != int(output.ExitOK) {
		t.Fatalf("audit append failed")
	}

	stdout, _, _ := runCLI(t,
		"status",
		"--repo", repoURL,
		"-o", "json",
	)
	var res output.Result
	stdjson.Unmarshal([]byte(stdout), &res)
	body, _ := stdjson.Marshal(res.Result)
	bodyStr := string(body)
	if !strings.Contains(bodyStr, `"chain_event_count":1`) {
		t.Errorf("expected chain_event_count=1: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, `"present":false`) {
		t.Errorf("anchor should NOT be present (no anchor written): %s", bodyStr)
	}
	if !strings.Contains(bodyStr, `"fresh":false`) {
		t.Errorf("anchor should be reported as not fresh: %s", bodyStr)
	}
}

// TestStatus_PendingApprovalsCount: an outstanding (un-approved)
// request bumps pending_approvals.
func TestStatus_PendingApprovalsCount(t *testing.T) {
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

	if _, _, exit := runCLI(t,
		"approval", "request",
		"--repo", repoURL,
		"--op", "repo.gc",
		"--target", repoURL,
		"--threshold", "2",
		"--approver-key", pubA,
		"--approver-key", pubB,
		"-o", "json",
	); exit != int(output.ExitOK) {
		t.Fatalf("approval request failed")
	}

	stdout, _, _ := runCLI(t,
		"status",
		"--repo", repoURL,
		"-o", "json",
	)
	var res output.Result
	stdjson.Unmarshal([]byte(stdout), &res)
	body, _ := stdjson.Marshal(res.Result)
	if !strings.Contains(string(body), `"pending_approvals":1`) {
		t.Errorf("expected pending_approvals=1: %s", body)
	}
}

// TestStatus_TextRendererShowsAnchorAndApprovals: the operator-
// facing text body surfaces the new repo-level footer.
func TestStatus_TextRendererShowsAnchorAndApprovals(t *testing.T) {
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	repoURL := "file://" + repoDir
	if _, _, exit := runCLI(t, "repo", "init", repoURL); exit != int(output.ExitOK) {
		t.Fatalf("repo init failed")
	}

	stdout, _, exit := runCLI(t,
		"status",
		"--repo", repoURL,
		"-o", "text",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("status: exit=%d\nstdout=%s", exit, stdout)
	}
	for _, want := range []string{
		"Audit anchor:",
		"chain empty",
		"Pending approvals: 0",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("text render missing %q:\n%s", want, stdout)
		}
	}
}
