package cli_test

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// TestVerify_EmitsAuditRun pins the compliance-reconciliation fix: a verify
// run now writes a `verify.run` audit event so the compliance report's
// verification section (SOC 2 A1.2 / ISO A.8.13) can roll it up. Before the
// fix the verify command persisted nothing to the audit chain — the
// verification control therefore always read zero runs regardless of how
// many verifications an operator ran.
func TestVerify_EmitsAuditRun(t *testing.T) {
	w := newReadWorld(t)
	id := commitVerifiableBackup(t, w, "db1", 0, []byte("payload"))

	if _, _, exit := runCLI(t, "verify", "db1", id,
		"--repo", w.repoURL, "-o", "json"); exit != int(output.ExitOK) {
		t.Fatalf("verify exit=%d", exit)
	}

	// The audit chain now carries a verify.run record for the run.
	out, _, exit := runCLI(t, "audit", "search",
		"--repo", w.repoURL, "--action", "verify.run", "--output", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("audit search exit=%d", exit)
	}
	for _, want := range []string{`"count": 1`, `"action": "verify.run"`, id} {
		if !strings.Contains(out, want) {
			t.Errorf("verify.run audit missing %q:\n%s", want, out)
		}
	}
}
