package cli_test

import (
	stdjson "encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// approvedGCRequest files and fully approves a repo.gc request bound to
// repoURL, signed by the given (priv, pub) key paths.
func approvedGCRequest(t *testing.T, repoURL string, keys ...[2]string) string {
	t.Helper()
	args := []string{"approval", "request", "--repo", repoURL, "--op", "repo.gc",
		"--target", repoURL, "--threshold", "1", "-o", "json"}
	for _, k := range keys {
		args = append(args, "--approver-key", k[1])
	}
	stdout, stderr, exit := runCLI(t, args...)
	if exit != int(output.ExitOK) {
		t.Fatalf("request: exit=%d\n%s\n%s", exit, stdout, stderr)
	}
	var reqRes output.Result
	if err := stdjson.Unmarshal([]byte(stdout), &reqRes); err != nil {
		t.Fatal(err)
	}
	id := reqRes.Result.(map[string]any)["id"].(string)
	for _, k := range keys {
		if _, errb, exit := runCLI(t, "approval", "approve", id, "--repo", repoURL,
			"--key", k[0], "--approver", "x", "-o", "json"); exit != int(output.ExitOK) {
			t.Fatalf("approve: %s", errb)
		}
	}
	return id
}

// TestApproval_CLI_ApprovalIsSingleUse (C6): one approval authorises
// one destructive run. Before the fix the same request ID redeemed
// `repo gc --apply` (and wipe / shred / delete) any number of times.
func TestApproval_CLI_ApprovalIsSingleUse(t *testing.T) {
	tmp := t.TempDir()
	repoURL := "file://" + filepath.Join(tmp, "repo")
	if _, _, exit := runCLI(t, "repo", "init", repoURL); exit != int(output.ExitOK) {
		t.Fatalf("repo init failed")
	}
	privA, pubA := genApproverKeys(t, tmp, "alice")
	id := approvedGCRequest(t, repoURL, [2]string{privA, pubA})

	if _, errb, exit := runCLI(t, "repo", "gc", repoURL, "--apply",
		"--require-approval", id, "-o", "json"); exit != int(output.ExitOK) {
		t.Fatalf("first redemption: exit=%d\n%s", exit, errb)
	}
	_, errb, exit := runCLI(t, "repo", "gc", repoURL, "--apply",
		"--require-approval", id, "-o", "json")
	if exit == int(output.ExitOK) {
		t.Fatal("the same approval authorised a second repo gc --apply")
	}
	if !strings.Contains(errb, "single-use") {
		t.Errorf("refusal should say the approval was already redeemed: %s", errb)
	}

	stdout, _, _ := runCLI(t, "approval", "status", id, "--repo", repoURL, "-o", "json")
	if !strings.Contains(stdout, `"consumed_at"`) {
		t.Errorf("approval status does not show the redemption: %s", stdout)
	}
}

// TestApproval_CLI_RequiresTarget (C6): every gated op acts on a
// target, and the gate now demands an exact match, so a request filed
// without one is refused at creation instead of being unredeemable.
func TestApproval_CLI_RequiresTarget(t *testing.T) {
	tmp := t.TempDir()
	repoURL := "file://" + filepath.Join(tmp, "repo")
	if _, _, exit := runCLI(t, "repo", "init", repoURL); exit != int(output.ExitOK) {
		t.Fatalf("repo init failed")
	}
	_, pubA := genApproverKeys(t, tmp, "alice")
	_, errb, exit := runCLI(t, "approval", "request", "--repo", repoURL,
		"--op", "repo.wipe", "--threshold", "1", "--approver-key", pubA, "-o", "json")
	if exit != int(output.ExitMisuse) || !strings.Contains(errb, "--target") {
		t.Fatalf("target-less request: exit=%d, want %d naming --target\n%s", exit, output.ExitMisuse, errb)
	}
}

// TestApproval_CLI_RefusesUntrustedApproverKey (H17): the request
// creator may no longer pick the approver roster. A key that is not on
// the operator's trusted roster is refused at request time (auth.*,
// exit 3) ...
func TestApproval_CLI_RefusesUntrustedApproverKey(t *testing.T) {
	tmp := t.TempDir()
	repoURL := "file://" + filepath.Join(tmp, "repo")
	if _, _, exit := runCLI(t, "repo", "init", repoURL); exit != int(output.ExitOK) {
		t.Fatalf("repo init failed")
	}
	_, _ = genApproverKeys(t, tmp, "alice") // configures a roster
	_, pubM := genUntrustedApproverKeys(t, tmp, "mallory")

	_, errb, exit := runCLI(t, "approval", "request", "--repo", repoURL,
		"--op", "repo.wipe", "--target", repoURL, "--threshold", "1",
		"--approver-key", pubM, "-o", "json")
	if exit != int(output.ExitAuth) || !strings.Contains(errb, "auth.approver_untrusted") {
		t.Fatalf("self-chosen approver key: exit=%d, want %d auth.approver_untrusted\n%s", exit, output.ExitAuth, errb)
	}
}

// TestApproval_CLI_GateIgnoresPlantedSelfApproval (H17): a request
// planted directly in the repository (bypassing the CLI's request-time
// check) still cannot be redeemed, because the gate counts only votes
// from the trusted roster.
func TestApproval_CLI_GateIgnoresPlantedSelfApproval(t *testing.T) {
	tmp := t.TempDir()
	repoURL := "file://" + filepath.Join(tmp, "repo")
	if _, _, exit := runCLI(t, "repo", "init", repoURL); exit != int(output.ExitOK) {
		t.Fatalf("repo init failed")
	}
	privM, pubM := genApproverKeys(t, tmp, "mallory")
	id := approvedGCRequest(t, repoURL, [2]string{privM, pubM})
	// The operator's roster, configured afterwards, does not include
	// mallory: her request and vote are exactly what a repo-write
	// attacker could plant.
	_, _ = genApproverKeys(t, t.TempDir(), "alice")

	if _, errb, exit := runCLI(t, "repo", "gc", repoURL, "--apply",
		"--require-approval", id, "-o", "json"); exit == int(output.ExitOK) {
		t.Fatalf("a self-approved request outside the trusted roster authorised repo gc --apply\n%s", errb)
	}
}
