package cli_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	stdjson "encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// genApproverKeys writes <prefix>.pub (PEM-encoded public key) +
// <prefix>.key (PEM-encoded private key) under tmp and returns the
// two paths.
func genApproverKeys(t *testing.T, tmp, prefix string) (privPath, pubPath string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "PG_HARDSTORAGE ED25519 PRIVATE KEY", Bytes: privDER})
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PG_HARDSTORAGE ED25519 PUBLIC KEY", Bytes: pubDER})
	privPath = filepath.Join(tmp, prefix+".key")
	pubPath = filepath.Join(tmp, prefix+".pub")
	if err := os.WriteFile(privPath, privPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pubPath, pubPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	return privPath, pubPath
}

// runCLI is shared with listshowstatus_test.go (same package).

// TestApproval_CLI_RequestApproveStatus exercises the full
// request → approve → approve → status flow against an fs:// repo.
// First approval keeps status pending (threshold 2); second approval
// flips to approved.
func TestApproval_CLI_RequestApproveStatus(t *testing.T) {
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	repoURL := "file://" + repoDir

	// Initialise the repo so audit.Append doesn't choke.
	stdout, stderr, exit := runCLI(t, "repo", "init", repoURL)
	if exit != int(output.ExitOK) {
		t.Fatalf("repo init: exit=%d\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}

	privA, pubA := genApproverKeys(t, tmp, "alice")
	privB, pubB := genApproverKeys(t, tmp, "bob")

	// Request: backup.delete, threshold 2, two approver keys.
	stdout, stderr, exit = runCLI(t,
		"approval", "request",
		"--repo", repoURL,
		"--op", "backup.delete",
		"--target", "db1.full.20260427T0900Z",
		"--reason", "old monthly retention",
		"--threshold", "2",
		"--approver-key", pubA,
		"--approver-key", pubB,
		"-o", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("approval request: exit=%d\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}
	var reqRes output.Result
	if err := stdjson.Unmarshal([]byte(stdout), &reqRes); err != nil {
		t.Fatalf("decode request: %v\n%s", err, stdout)
	}
	resultMap, _ := reqRes.Result.(map[string]any)
	requestID, _ := resultMap["id"].(string)
	if requestID == "" {
		t.Fatalf("no request ID in result: %+v", reqRes.Result)
	}

	// First approval (alice) — status should still be pending.
	stdout, stderr, exit = runCLI(t,
		"approval", "approve", requestID,
		"--repo", repoURL,
		"--key", privA,
		"--approver", "alice@acme.example",
		"-o", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("alice approve: exit=%d\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}
	if !strings.Contains(stdout, `"status": "pending"`) {
		t.Errorf("after alice: status not pending in result: %s", stdout)
	}

	// Second approval (bob) — should flip status to approved.
	stdout, stderr, exit = runCLI(t,
		"approval", "approve", requestID,
		"--repo", repoURL,
		"--key", privB,
		"--approver", "bob@acme.example",
		"-o", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("bob approve: exit=%d\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}
	if !strings.Contains(stdout, `"status": "approved"`) {
		t.Errorf("after bob: status not approved: %s", stdout)
	}

	// Status command — should show 2/2 approved.
	stdout, stderr, exit = runCLI(t,
		"approval", "status", requestID,
		"--repo", repoURL,
		"-o", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("status: exit=%d\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}
	if !strings.Contains(stdout, `"approval_count": 2`) {
		t.Errorf("approval_count != 2: %s", stdout)
	}
	if !strings.Contains(stdout, `"status": "approved"`) {
		t.Errorf("status: %s", stdout)
	}

	// List command — should include the request.
	stdout, stderr, exit = runCLI(t,
		"approval", "list",
		"--repo", repoURL,
		"-o", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("list: exit=%d\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}
	if !strings.Contains(stdout, requestID) {
		t.Errorf("list missing request: %s", stdout)
	}
}

// TestApproval_CLI_NotAllowedKey asserts the auth.approver_not_allowed
// error code surfaces when the approver's key isn't in the allowlist.
func TestApproval_CLI_NotAllowedKey(t *testing.T) {
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	repoURL := "file://" + repoDir

	if _, _, exit := runCLI(t, "repo", "init", repoURL); exit != int(output.ExitOK) {
		t.Fatalf("repo init failed")
	}

	_, pubA := genApproverKeys(t, tmp, "allowed")
	privUnlisted, _ := genApproverKeys(t, tmp, "mallory") // NOT in the request

	// Request lists only pubA in approvers.
	stdout, stderr, exit := runCLI(t,
		"approval", "request",
		"--repo", repoURL,
		"--op", "backup.delete",
		"--threshold", "1",
		"--approver-key", pubA,
		"-o", "json",
	)
	if exit != int(output.ExitOK) {
		t.Fatalf("request: %s\n%s", stdout, stderr)
	}
	var reqRes output.Result
	stdjson.Unmarshal([]byte(stdout), &reqRes)
	requestID := reqRes.Result.(map[string]any)["id"].(string)

	// Mallory tries to approve; the CLI should refuse with the
	// structured "approver not allowed" code, mapping to ExitAuth.
	_, stderr, exit = runCLI(t,
		"approval", "approve", requestID,
		"--repo", repoURL,
		"--key", privUnlisted,
		"--approver", "mallory@evil",
		"-o", "json",
	)
	if exit != int(output.ExitAuth) {
		t.Errorf("expected ExitAuth(%d); got %d\nstderr=%s", output.ExitAuth, exit, stderr)
	}
	if !strings.Contains(stderr, "auth.approver_not_allowed") {
		t.Errorf("expected auth.approver_not_allowed code; stderr=%s", stderr)
	}
}
