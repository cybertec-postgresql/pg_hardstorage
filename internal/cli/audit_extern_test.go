package cli_test

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

func TestAudit_Append_HappyPath(t *testing.T) {
	repoURL := initRepoForTest(t)
	out, _, exit := runCmd(t,
		"audit", "append", "kms.rotate",
		"--repo", repoURL,
		"--actor", "ops@acme",
		"--reason", "scheduled rotation",
		"--output", "json",
	)
	if exit != 0 {
		t.Fatalf("exit = %d, out:\n%s", exit, out)
	}
	for _, want := range []string{
		`"action": "kms.rotate"`,
		`"sequence": 0`,
		`"prev_hash": "0000000000000000000000000000000000000000000000000000000000000000"`,
		`"hash": "`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestAudit_Append_RequiresRepo(t *testing.T) {
	_, _, exit := runCmd(t, "audit", "append", "x", "--output", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want ExitMisuse", exit)
	}
}

func TestAudit_Search_FiltersByAction(t *testing.T) {
	repoURL := initRepoForTest(t)
	for _, action := range []string{"backup.create", "kms.rotate", "backup.create"} {
		_, _, exit := runCmd(t,
			"audit", "append", action,
			"--repo", repoURL, "--output", "json",
		)
		if exit != 0 {
			t.Fatalf("append %q: exit %d", action, exit)
		}
	}
	out, _, exit := runCmd(t,
		"audit", "search",
		"--repo", repoURL, "--action", "backup.create",
		"--output", "json",
	)
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	if !strings.Contains(out, `"count": 2`) {
		t.Errorf("filter --action backup.create should match 2; got:\n%s", out)
	}
}

func TestAudit_VerifyChain_HappyPath(t *testing.T) {
	repoURL := initRepoForTest(t)
	for i := 0; i < 5; i++ {
		_, _, exit := runCmd(t,
			"audit", "append", "test.tick",
			"--repo", repoURL, "--output", "json",
		)
		if exit != 0 {
			t.Fatalf("append %d: %d", i, exit)
		}
	}
	out, _, exit := runCmd(t,
		"audit", "verify-chain",
		"--repo", repoURL, "--output", "json",
	)
	if exit != 0 {
		t.Fatalf("exit = %d, out:\n%s", exit, out)
	}
	for _, want := range []string{
		`"events_checked": 5`,
		`"ok": true`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestAudit_Search_AcceptsDurationSince(t *testing.T) {
	// `--since 24h` should be accepted (parsed as duration).
	repoURL := initRepoForTest(t)
	_, _, exit := runCmd(t,
		"audit", "append", "test.tick",
		"--repo", repoURL, "--output", "json",
	)
	if exit != 0 {
		t.Fatal("seed append failed")
	}
	out, _, exit := runCmd(t,
		"audit", "search",
		"--repo", repoURL, "--since", "24h",
		"--output", "json",
	)
	if exit != 0 {
		t.Fatalf("exit = %d, out:\n%s", exit, out)
	}
	if !strings.Contains(out, `"count": 1`) {
		t.Errorf("--since 24h should match the just-appended event:\n%s", out)
	}
}

func TestAudit_Search_RejectsBadSince(t *testing.T) {
	repoURL := initRepoForTest(t)
	_, _, exit := runCmd(t,
		"audit", "search",
		"--repo", repoURL, "--since", "not-a-time",
		"--output", "json",
	)
	if exit != int(output.ExitMisuse) {
		t.Errorf("bad --since should exit ExitMisuse; got %d", exit)
	}
}
