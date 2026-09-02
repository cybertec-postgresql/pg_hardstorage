package cli_test

// "I could not look" must not render as "there is nothing there".
//
// countPendingApprovals collapsed any failure walking the approvals/
// prefix into 0, and status renders a bare "Pending approvals: 0". So a
// repository whose approval listing could not be read told the operator,
// on the primary at-a-glance screen, that nothing awaited sign-off --
// the one conclusion that is unsafe to act on.
//
// The listing lost requests because approval.Store.List swallowed every
// per-request fetch error with a bare `continue`, so a single corrupt
// body silently subtracted itself from the count.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

func TestStatus_UnreadableApprovalIsNotReportedAsZeroPending(t *testing.T) {
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	repoURL := "file://" + repoDir
	if _, _, exit := runCLI(t, "repo", "init", repoURL); exit != int(output.ExitOK) {
		t.Fatal("repo init failed")
	}
	_, pubA := genApproverKeys(t, tmp, "alice")
	_, pubB := genApproverKeys(t, tmp, "bob")

	if _, _, exit := runCLI(t,
		"approval", "request", "--repo", repoURL,
		"--op", "repo.gc", "--target", repoURL,
		"--threshold", "2",
		"--approver-key", pubA, "--approver-key", pubB,
		"-o", "json",
	); exit != int(output.ExitOK) {
		t.Fatal("approval request failed")
	}

	// Sanity: with the body intact, status sees exactly one pending.
	out, _, exit := runCLI(t, "status", "--repo", repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("status failed: %s", out)
	}
	if got := pendingFromJSON(t, out); got != 1 {
		t.Fatalf("precondition: pending_approvals = %d, want 1", got)
	}
	if unknownFromJSON(t, out) {
		t.Fatal("precondition: pending_approvals_unknown set on a clean repo")
	}

	// Corrupt the approval body, leaving the key in place.
	approvalsDir := filepath.Join(repoDir, "approvals")
	entries, err := os.ReadDir(approvalsDir)
	if err != nil {
		t.Fatalf("read approvals dir: %v", err)
	}
	var corrupted int
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if err := os.WriteFile(filepath.Join(approvalsDir, e.Name()),
			[]byte("{ not json at all"), 0o644); err != nil {
			t.Fatal(err)
		}
		corrupted++
	}
	if corrupted == 0 {
		t.Fatal("no approval body found to corrupt; the on-disk layout changed")
	}

	out, _, exit = runCLI(t, "status", "--repo", repoURL, "-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("status should still render: %s", out)
	}
	if !unknownFromJSON(t, out) {
		t.Errorf("pending_approvals_unknown is false after the only approval body was "+
			"made unreadable.\n\n%s\n\nThe count is a floor, not a count, and the report "+
			"must say so; otherwise a corrupt or tampered approval subtracts itself from "+
			"the operator's view of what awaits sign-off.", out)
	}

	// And the human-readable form must not print a bare zero.
	text, _, exit := runCLI(t, "status", "--repo", repoURL, "-o", "text")
	if exit != int(output.ExitOK) {
		t.Fatalf("status text failed: %s", text)
	}
	if strings.Contains(text, "Pending approvals: 0\n") {
		t.Errorf("status printed a flat \"Pending approvals: 0\" for a listing it could "+
			"not read:\n\n%s", text)
	}
	t.Logf("TEXT OUTPUT:\n%s", text)
	if !strings.Contains(text, "COULD NOT BE READ") {
		t.Errorf("status gave no sign the approval listing was incomplete:\n\n%s", text)
	}
}

// statusBodyOf re-marshals the status result body the same way the
// other status tests do (the payload sits at Result.Result).
func statusBodyOf(t *testing.T, out string) struct {
	PendingApprovals int  `json:"pending_approvals"`
	Unknown          bool `json:"pending_approvals_unknown"`
} {
	t.Helper()
	var res output.Result
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("parse status envelope: %v\n%s", err, out)
	}
	raw, err := json.Marshal(res.Result)
	if err != nil {
		t.Fatalf("re-marshal status body: %v", err)
	}
	var body struct {
		PendingApprovals int  `json:"pending_approvals"`
		Unknown          bool `json:"pending_approvals_unknown"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("parse status body: %v\n%s", err, raw)
	}
	return body
}

func pendingFromJSON(t *testing.T, out string) int {
	t.Helper()
	return statusBodyOf(t, out).PendingApprovals
}

func unknownFromJSON(t *testing.T, out string) bool {
	t.Helper()
	return statusBodyOf(t, out).Unknown
}
