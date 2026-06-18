package cli_test

import (
	stdjson "encoding/json"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
)

// TestGameDay_List: cobra-level smoke — at least one scenario is
// registered (the in-tree agent_kill / s3_throttle / patroni_failover
// trio) and the body decodes cleanly.
func TestGameDay_List(t *testing.T) {
	stdout, _, exit := runCmd(t, "gameday", "list", "--output", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("gameday list: exit=%d\n%s", exit, stdout)
	}
	var view struct {
		Scenarios []struct {
			Name string `json:"name"`
		} `json:"scenarios"`
	}
	bodyOf(t, stdout, &view)
	if len(view.Scenarios) == 0 {
		t.Errorf("expected at least one registered scenario; got 0")
	}
	// Sanity: the named-in-the-plan trio is present.
	have := map[string]bool{}
	for _, s := range view.Scenarios {
		have[s.Name] = true
	}
	for _, want := range []string{"agent_kill", "s3_throttle", "patroni_failover"} {
		if !have[want] {
			t.Errorf("scenario %q missing from registry", want)
		}
	}
}

// TestGameDay_RunUnknown: a non-existent scenario maps to
// notfound.scenario with the registered list in the suggestion.
func TestGameDay_RunUnknown(t *testing.T) {
	_, stderr, exit := runCmd(t,
		"gameday", "run", "definitely-not-real",
		"--output", "json")
	if exit == int(output.ExitOK) {
		t.Errorf("unknown scenario should not exit OK; stderr=%s", stderr)
	}
	if !strings.Contains(stderr, "notfound.scenario") {
		t.Errorf("expected notfound.scenario code:\n%s", stderr)
	}
}

// TestGameDay_RunWithoutRepo: --repo is OPTIONAL (the operator may
// be running ad-hoc without an audit-chain target). The run should
// pass cleanly and emit no audit event.
func TestGameDay_RunWithoutRepo(t *testing.T) {
	stdout, _, exit := runCmd(t,
		"gameday", "run", "agent_kill",
		"--dry-run", "--output", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("ad-hoc dry-run should pass; got %d\n%s", exit, stdout)
	}
	if !strings.Contains(stdout, `"pass": true`) {
		t.Errorf("expected pass=true:\n%s", stdout)
	}
}

// TestGameDay_RunEmitsAuditEvent: when --repo is set, a successful
// run appends a `gameday.run` event to the audit chain. Find it via
// `audit search --action gameday.run`.
func TestGameDay_RunEmitsAuditEvent(t *testing.T) {
	repoURL := initRepoForTest(t)
	stdout, _, exit := runCmd(t,
		"gameday", "run", "agent_kill",
		"--repo", repoURL,
		"--dry-run",
		"--output", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("run: exit=%d\n%s", exit, stdout)
	}
	// Walk the chain via audit search.
	auditOut, _, exit2 := runCmd(t,
		"audit", "search",
		"--repo", repoURL,
		"--action", "gameday.run",
		"--output", "json")
	if exit2 != int(output.ExitOK) {
		t.Fatalf("audit search: exit=%d\n%s", exit2, auditOut)
	}
	if !strings.Contains(auditOut, "gameday.run") {
		t.Errorf("expected gameday.run event in chain:\n%s", auditOut)
	}
	// The gameday.run event existence is the contract we care about
	// — the body's scenario field is verified more directly via
	// `gameday report` (which decodes the body), not via `audit
	// search` (which returns only the chain summary).
}

// TestGameDay_ReportRequiresRepo: --repo is mandatory for report
// (it's a chain walk; without a repo there's nothing to walk).
func TestGameDay_ReportRequiresRepo(t *testing.T) {
	_, stderr, exit := runCmd(t,
		"gameday", "report", "--output", "json")
	if exit != int(output.ExitMisuse) {
		t.Errorf("missing --repo should exit ExitMisuse; got %d\nstderr=%s",
			exit, stderr)
	}
	if !strings.Contains(stderr, "usage.missing_flag") {
		t.Errorf("expected usage.missing_flag: %s", stderr)
	}
}

// TestGameDay_ReportEmpty: a fresh repo with no gameday runs returns
// total_events=0 + an empty scenarios list.
func TestGameDay_ReportEmpty(t *testing.T) {
	repoURL := initRepoForTest(t)
	stdout, _, exit := runCmd(t,
		"gameday", "report", "--repo", repoURL, "--output", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("empty report: exit=%d\n%s", exit, stdout)
	}
	if !strings.Contains(stdout, `"total_events": 0`) {
		t.Errorf("expected total_events=0:\n%s", stdout)
	}
}

// TestGameDay_ReportAggregatesRuns: drive three dry-runs of
// agent_kill against the same repo, then assert the report shows
// scenario=agent_kill with total=3, passes=3, fails=0.
func TestGameDay_ReportAggregatesRuns(t *testing.T) {
	repoURL := initRepoForTest(t)
	for i := 0; i < 3; i++ {
		_, _, exit := runCmd(t,
			"gameday", "run", "agent_kill",
			"--repo", repoURL,
			"--dry-run",
			"--output", "json")
		if exit != int(output.ExitOK) {
			t.Fatalf("run %d: exit=%d", i, exit)
		}
	}
	stdout, _, exit := runCmd(t,
		"gameday", "report",
		"--repo", repoURL,
		"--output", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("report: exit=%d\n%s", exit, stdout)
	}
	// Decode the envelope, then re-marshal compactly so field+value
	// asserts don't have to account for indentation whitespace.
	var env output.Result
	if err := stdjson.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout)
	}
	body, _ := stdjson.Marshal(env.Result)
	for _, want := range []string{
		`"scenario":"agent_kill"`,
		`"total":3`,
		`"passes":3`,
		`"fails":0`,
		`"dry_runs":3`,
		`"most_recent_pass":true`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
}

// TestGameDay_ReportScenarioFilter: --scenario X narrows the
// per-scenario aggregate; events for other scenarios are excluded.
func TestGameDay_ReportScenarioFilter(t *testing.T) {
	repoURL := initRepoForTest(t)
	for _, name := range []string{"agent_kill", "s3_throttle"} {
		_, _, exit := runCmd(t,
			"gameday", "run", name,
			"--repo", repoURL,
			"--dry-run",
			"--output", "json")
		if exit != int(output.ExitOK) {
			t.Fatalf("run %s: exit=%d", name, exit)
		}
	}
	stdout, _, exit := runCmd(t,
		"gameday", "report",
		"--repo", repoURL,
		"--scenario", "s3_throttle",
		"--output", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("filtered report: exit=%d\n%s", exit, stdout)
	}
	// Body should mention s3_throttle but NOT agent_kill in the
	// scenarios summary.
	var env output.Result
	if err := stdjson.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout)
	}
	body, _ := stdjson.Marshal(env.Result)
	if !strings.Contains(string(body), `"scenario":"s3_throttle"`) {
		t.Errorf("expected s3_throttle in filtered report:\n%s", body)
	}
	if strings.Contains(string(body), `"scenario":"agent_kill"`) {
		t.Errorf("agent_kill leaked through scenario filter:\n%s", body)
	}
}

// TestGameDay_S3Throttle_DrivesFaultInjection: with --repo set and
// not a dry-run, runS3Throttle drives a real fault-injection
// against the backend (Put fails during the fault window, then
// succeeds after Deactivate). Evidence should reflect the
// observed timeline.
func TestGameDay_S3Throttle_DrivesFaultInjection(t *testing.T) {
	repoURL := initRepoForTest(t)
	stdout, _, exit := runCmd(t,
		"gameday", "run", "s3_throttle",
		"--repo", repoURL,
		"--fault-duration", "1s",
		"--output", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("s3_throttle should pass when fault-injection works; got %d\n%s",
			exit, stdout)
	}
	for _, want := range []string{
		`"pass": true`,
		`"fault_active"`,
		`"fault_observed"`,
		`"fault_cleared"`,
		`"recovered"`,
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("evidence missing %q:\n%s", want, stdout)
		}
	}
}

// TestGameDay_S3Throttle_NoRepoIsContractOnly: without --repo, the
// scenario falls back to the pass-by-contract path (no live fault
// injection, but the run still passes and records the invariant).
// This preserves the existing ad-hoc behaviour.
func TestGameDay_S3Throttle_NoRepoIsContractOnly(t *testing.T) {
	stdout, _, exit := runCmd(t,
		"gameday", "run", "s3_throttle",
		"--output", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("contract-only run should pass; got %d\n%s", exit, stdout)
	}
	if !strings.Contains(stdout, `"pass": true`) {
		t.Errorf("expected pass=true:\n%s", stdout)
	}
	// Contract-only run should NOT mention fault_active / fault_observed.
	if strings.Contains(stdout, `"fault_active"`) {
		t.Errorf("contract-only run should not drive fault injection:\n%s", stdout)
	}
}

// TestGameDay_ReportTextRender: the operator-friendly text output
// has the table header + per-scenario row.
func TestGameDay_ReportTextRender(t *testing.T) {
	repoURL := initRepoForTest(t)
	_, _, exit := runCmd(t,
		"gameday", "run", "agent_kill",
		"--repo", repoURL,
		"--dry-run",
		"--output", "json")
	if exit != int(output.ExitOK) {
		t.Fatalf("setup run failed: exit=%d", exit)
	}
	stdout, _, exit := runCmd(t,
		"gameday", "report",
		"--repo", repoURL,
		"--output", "text")
	if exit != int(output.ExitOK) {
		t.Fatalf("text report: exit=%d\n%s", exit, stdout)
	}
	for _, want := range []string{
		"gameday report",
		"SCENARIO",
		"agent_kill",
		"PASS",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("text report missing %q:\n%s", want, stdout)
		}
	}
}
