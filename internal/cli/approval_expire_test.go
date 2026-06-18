package cli_test

import (
	stdjson "encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// TestApprovalPurgeExpired_EmitsAuditEvent pins the approval.expire gap
// fix: recording expiry now writes an `approval.expire` audit event, so the
// compliance report's expired-request count can move off zero. Before the
// fix, expiry was a purely derived state with no chain record and the
// consumer case in the report never fired.
func TestApprovalPurgeExpired_EmitsAuditEvent(t *testing.T) {
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	repoURL := "file://" + repoDir
	if _, _, exit := runCLI(t, "repo", "init", repoURL); exit != int(output.ExitOK) {
		t.Fatalf("repo init exit=%d", exit)
	}
	_, pubA := genApproverKeys(t, tmp, "alice")

	// Request with a tiny TTL, then let it lapse.
	stdout, stderr, exit := runCLI(t, "approval", "request",
		"--repo", repoURL,
		"--op", "backup.delete",
		"--target", "db1.full.X",
		"--reason", "old monthly retention",
		"--threshold", "1",
		"--approver-key", pubA,
		"--ttl", "20ms",
		"-o", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("approval request exit=%d\n%s\n%s", exit, stdout, stderr)
	}
	var reqRes output.Result
	if err := stdjson.Unmarshal([]byte(stdout), &reqRes); err != nil {
		t.Fatalf("decode request: %v\n%s", err, stdout)
	}
	if rm, _ := reqRes.Result.(map[string]any); rm["id"] == "" {
		t.Fatalf("no request id: %s", stdout)
	}
	time.Sleep(80 * time.Millisecond)

	// Record expiry.
	if _, serr, exit := runCLI(t, "approval", "purge-expired",
		"--repo", repoURL, "--yes", "-o", "json"); exit != int(output.ExitOK) {
		t.Fatalf("approval purge-expired exit=%d\n%s", exit, serr)
	}

	// The audit chain now carries an approval.expire record.
	out, _, exit := runCLI(t, "audit", "search",
		"--repo", repoURL, "--action", "approval.expire", "--output", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("audit search exit=%d", exit)
	}
	for _, want := range []string{`"count": 1`, `"action": "approval.expire"`} {
		if !strings.Contains(out, want) {
			t.Errorf("approval.expire audit missing %q:\n%s", want, out)
		}
	}
}
