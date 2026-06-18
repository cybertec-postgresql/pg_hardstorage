package cli_test

import (
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// seedAuditEvents appends a varied set of events covering
// deployments, backup IDs, and action namespaces. Used by the
// new v0.6+ audit search/summary CLI tests.
func seedAuditEvents(t *testing.T, repoURL string) {
	t.Helper()
	for _, e := range []struct {
		action, actor, deployment string
	}{
		{"backup.create", "alice@acme.com", "db1"},
		{"backup.create", "bob@acme.com", "db2"},
		{"backup.delete", "alice@acme.com", "db1"},
		{"backup.delete", "bob@acme.com", "db2"},
		{"backup.undelete", "alice@acme.com", "db1"},
		{"kms.rotate", "system@cron", ""},
		{"hold.add", "compliance@acme.com", "db1"},
	} {
		args := []string{"audit", "append", e.action, "--repo", repoURL,
			"--actor", e.actor}
		if e.deployment != "" {
			args = append(args, "--deployment", e.deployment)
		}
		args = append(args, "--output", "json")
		_, _, exit := runCmd(t, args...)
		if exit != 0 {
			t.Fatalf("seed %s/%s: exit=%d", e.action, e.deployment, exit)
		}
	}
}

// TestAuditSearch_ActionPrefix: --action-prefix backup.
// captures the full namespace.
func TestAuditSearch_ActionPrefix(t *testing.T) {
	repoURL := initRepoForTest(t)
	seedAuditEvents(t, repoURL)

	stdout, _, exit := runCmd(t, "audit", "search",
		"--repo", repoURL, "--action-prefix", "backup.",
		"--output", "json")
	if exit != 0 {
		t.Fatalf("exit=%d\n%s", exit, stdout)
	}
	if !strings.Contains(stdout, `"count": 5`) {
		t.Errorf("--action-prefix backup. should match 5 (2 create + 2 delete + 1 undelete); got:\n%s", stdout)
	}
}

// TestAuditSearch_DeploymentFilter: --deployment narrows to
// one deployment's events across all action namespaces.
func TestAuditSearch_DeploymentFilter(t *testing.T) {
	repoURL := initRepoForTest(t)
	seedAuditEvents(t, repoURL)

	stdout, _, exit := runCmd(t, "audit", "search",
		"--repo", repoURL, "--deployment", "db1",
		"--output", "json")
	if exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
	// db1 events: create + delete + undelete + hold.add = 4
	if !strings.Contains(stdout, `"count": 4`) {
		t.Errorf("--deployment db1 should match 4; got:\n%s", stdout)
	}
	// Per-row deployment field populated.
	if !strings.Contains(stdout, `"deployment": "db1"`) {
		t.Errorf("expected db1 in row body:\n%s", stdout)
	}
}

// TestAuditSearch_ActorContains: --actor-contains substring
// filter.
func TestAuditSearch_ActorContains(t *testing.T) {
	repoURL := initRepoForTest(t)
	seedAuditEvents(t, repoURL)

	stdout, _, exit := runCmd(t, "audit", "search",
		"--repo", repoURL, "--actor-contains", "@acme.com",
		"--output", "json")
	if exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
	// 6 acme events; system@cron excluded.
	if !strings.Contains(stdout, `"count": 6`) {
		t.Errorf("--actor-contains @acme.com should match 6; got:\n%s", stdout)
	}
}

// TestAuditSearch_Reverse_LimitGivesNewestN: --reverse + --limit
// 3 returns the 3 most-recent matching events. Validates the
// "what happened recently?" pagination shape.
func TestAuditSearch_Reverse_LimitGivesNewestN(t *testing.T) {
	repoURL := initRepoForTest(t)
	seedAuditEvents(t, repoURL)

	stdout, _, exit := runCmd(t, "audit", "search",
		"--repo", repoURL, "--reverse", "--limit", "3",
		"--output", "json")
	if exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
	if !strings.Contains(stdout, `"count": 3`) {
		t.Errorf("--reverse --limit 3 should match 3; got:\n%s", stdout)
	}
	// The newest event in the seed is hold.add — it must
	// appear first in the array.
	idxHold := strings.Index(stdout, `"action": "hold.add"`)
	idxKMS := strings.Index(stdout, `"action": "kms.rotate"`)
	if idxHold < 0 || idxKMS < 0 || idxHold > idxKMS {
		t.Errorf("expected hold.add (newest) before kms.rotate in reverse output:\n%s", stdout)
	}
}

// TestAuditSummary_GroupsByAction: the new audit summary
// subcommand returns counts per action plus a total.
func TestAuditSummary_GroupsByAction(t *testing.T) {
	repoURL := initRepoForTest(t)
	seedAuditEvents(t, repoURL)

	stdout, _, exit := runCmd(t, "audit", "summary",
		"--repo", repoURL, "--output", "json")
	if exit != 0 {
		t.Fatalf("exit=%d\n%s", exit, stdout)
	}
	for _, want := range []string{
		`"total": 7`,
		`"action": "backup.create"`,
		`"count": 2`,
		`"action": "backup.delete"`,
		`"action": "backup.undelete"`,
		`"action": "hold.add"`,
		`"action": "kms.rotate"`,
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("audit summary missing %q:\n%s", want, stdout)
		}
	}
}

// TestAuditSummary_FiltersBeforeGrouping: --deployment narrows
// the rollup to db1's events.
func TestAuditSummary_FiltersBeforeGrouping(t *testing.T) {
	repoURL := initRepoForTest(t)
	seedAuditEvents(t, repoURL)

	stdout, _, exit := runCmd(t, "audit", "summary",
		"--repo", repoURL, "--deployment", "db1",
		"--output", "json")
	if exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
	if !strings.Contains(stdout, `"total": 4`) {
		t.Errorf("--deployment db1 summary total should be 4; got:\n%s", stdout)
	}
	if strings.Contains(stdout, `"action": "kms.rotate"`) {
		t.Errorf("kms.rotate should not be in db1-scoped summary:\n%s", stdout)
	}
	if !strings.Contains(stdout, `"deployment": "db1"`) {
		t.Errorf("filters echo block should record deployment=db1:\n%s", stdout)
	}
}

// TestAuditSummary_NoMatches_ReturnsTotalZero: an over-narrow
// filter returns total=0 cleanly without erroring.
func TestAuditSummary_NoMatches_ReturnsTotalZero(t *testing.T) {
	repoURL := initRepoForTest(t)
	seedAuditEvents(t, repoURL)

	stdout, _, exit := runCmd(t, "audit", "summary",
		"--repo", repoURL, "--deployment", "no-such-deployment",
		"--output", "json")
	if exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
	if !strings.Contains(stdout, `"total": 0`) {
		t.Errorf("over-narrow filter should give total=0; got:\n%s", stdout)
	}
}

// TestAuditSummary_TextRendering: text mode emits a tabular
// view sorted by count (descending).
func TestAuditSummary_TextRendering(t *testing.T) {
	repoURL := initRepoForTest(t)
	seedAuditEvents(t, repoURL)

	stdout, _, exit := runCmd(t, "audit", "summary",
		"--repo", repoURL, "--output", "text")
	if exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
	for _, want := range []string{
		"audit event(s)",
		"grouped by action",
		"backup.create",
		"backup.delete",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("text missing %q:\n%s", want, stdout)
		}
	}
	// backup.create (count 2) should appear before
	// kms.rotate (count 1) in descending-by-count order.
	idxCreate := strings.Index(stdout, "backup.create")
	idxKMS := strings.Index(stdout, "kms.rotate")
	if idxCreate < 0 || idxKMS < 0 || idxCreate > idxKMS {
		t.Errorf("expected backup.create (count 2) before kms.rotate (count 1):\n%s", stdout)
	}
}

// TestAuditSearch_DefaultBodyShape_BackupIDOmittedWhenEmpty:
// regression — the default body still has backup_id as
// omitempty. Schema-additive only.
func TestAuditSearch_DefaultBodyShape_BackupIDOmittedWhenEmpty(t *testing.T) {
	repoURL := initRepoForTest(t)
	// Append one event WITHOUT a deployment/backup-id.
	_, _, exit := runCmd(t, "audit", "append", "kms.rotate",
		"--repo", repoURL, "--output", "json")
	if exit != 0 {
		t.Fatalf("append exit=%d", exit)
	}
	stdout, _, exit := runCmd(t, "audit", "search",
		"--repo", repoURL, "--output", "json")
	if exit != 0 {
		t.Fatalf("search exit=%d", exit)
	}
	if strings.Contains(stdout, `"backup_id"`) {
		t.Errorf("event without backup_id should not include the key (omitempty):\n%s", stdout)
	}
}

// TestAuditSearch_SummaryFlagDiscoverable: search and summary
// help advertise the new flags.
func TestAuditSearch_SummaryFlagDiscoverable(t *testing.T) {
	stdout, _, _ := runCmd(t, "audit", "search", "--help")
	for _, want := range []string{"--action-prefix", "--deployment", "--backup-id", "--actor-contains", "--reverse"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("audit search --help missing %q:\n%s", want, stdout)
		}
	}
	stdout, _, exit := runCmd(t, "audit", "summary", "--help")
	if exit != 0 {
		t.Errorf("audit summary --help exit=%d", exit)
	}
	for _, want := range []string{"--action-prefix", "--deployment", "--backup-id"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("audit summary --help missing %q:\n%s", want, stdout)
		}
	}
}

// TestAuditSummary_RequiresRepo: structured usage error for
// missing --repo.
func TestAuditSummary_RequiresRepo(t *testing.T) {
	_, _, exit := runCmd(t, "audit", "summary", "--output", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("missing --repo: exit=%d, want ExitMisuse", exit)
	}
}
