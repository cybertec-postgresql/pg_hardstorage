package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedTwoDeployments writes a config with two deployments —
// db1 has both backup + rotate schedules, db2 has only backup.
// Used to assert the fleet listing surfaces every (deployment,
// task) pair including the unset ones.
func seedTwoDeployments(t *testing.T, configDir string) {
	t.Helper()
	body := `schema: pg_hardstorage.config.v1
deployments:
  db1:
    pg_connection: postgres://x@h/db1
    repo: file:///tmp/x
    schedule:
      backup: { every: "6h" }
      rotate: { daily_at: "04:00" }
  db2:
    pg_connection: postgres://x@h/db2
    repo: file:///tmp/x
    schedule:
      backup: { every: "12h" }
`
	if err := os.WriteFile(filepath.Join(configDir, "pg_hardstorage.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestScheduleList_FleetView_JSON: zero-arg `schedule` returns
// every (deployment, task) pair in deterministic order. The
// rotate task on db2 is unset and surfaces as an empty Spec
// + omitted description.
func TestScheduleList_FleetView_JSON(t *testing.T) {
	dir := configDir(t)
	seedTwoDeployments(t, dir)

	stdout, _, exit := runCmd(t, "schedule", "--output", "json")
	if exit != 0 {
		t.Fatalf("schedule (no args): exit=%d\n%s", exit, stdout)
	}
	for _, want := range []string{
		`"deployment": "db1"`,
		`"deployment": "db2"`,
		`"task": "backup"`,
		`"task": "rotate"`,
		`"every": "6h"`,
		`"every": "12h"`,
		`"daily_at": "04:00"`,
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q in:\n%s", want, stdout)
		}
	}
	// db2's rotate task is unset — the row exists but
	// without a description.
	// Expect "db2" appears twice (backup + rotate rows).
	if strings.Count(stdout, `"deployment": "db2"`) != 2 {
		t.Errorf("expected 2 db2 rows (backup + rotate); got:\n%s", stdout)
	}
}

// TestScheduleList_TextRendering: text mode emits a header
// with the deployment count + a tabular per-row view. Unset
// schedules show as "off" so a gap is visible.
func TestScheduleList_TextRendering(t *testing.T) {
	dir := configDir(t)
	seedTwoDeployments(t, dir)

	stdout, _, exit := runCmd(t, "schedule", "--output", "text")
	if exit != 0 {
		t.Fatalf("text exit=%d\n%s", exit, stdout)
	}
	for _, want := range []string{
		"Schedules for 2 deployment(s)",
		"DEPLOYMENT",
		"TASK",
		"WHEN",
		"db1",
		"db2",
		"backup",
		"rotate",
		"off", // db2's rotate is unset
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q in text:\n%s", want, stdout)
		}
	}
}

// TestScheduleList_NoDeployments: an empty config emits a
// friendly "No deployments configured." line.
func TestScheduleList_NoDeployments(t *testing.T) {
	dir := configDir(t)
	body := `schema: pg_hardstorage.config.v1
deployments: {}
`
	if err := os.WriteFile(filepath.Join(dir, "pg_hardstorage.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	stdout, _, exit := runCmd(t, "schedule", "--output", "text")
	if exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
	if !strings.Contains(stdout, "No deployments configured") {
		t.Errorf("expected friendly empty message:\n%s", stdout)
	}
}

// TestScheduleList_ExistingShowSetUnchanged: the fleet
// listing must not break the existing positional-arg
// behavior. `schedule db1` still shows db1's schedule;
// `schedule db1 "every 30m"` still sets it.
func TestScheduleList_ExistingShowSetUnchanged(t *testing.T) {
	dir := configDir(t)
	seedTwoDeployments(t, dir)

	// Show one.
	stdout, _, exit := runCmd(t, "schedule", "db1", "--output", "json")
	if exit != 0 {
		t.Fatalf("schedule db1: exit=%d", exit)
	}
	if !strings.Contains(stdout, `"deployment": "db1"`) {
		t.Errorf("expected db1 in single-show:\n%s", stdout)
	}
	// Show shouldn't include other deployments' rows.
	if strings.Contains(stdout, `"deployment": "db2"`) {
		t.Errorf("single-show leaked other deployment:\n%s", stdout)
	}

	// Set one.
	_, _, exit = runCmd(t, "schedule", "db1", "every 30m", "--output", "json")
	if exit != 0 {
		t.Fatalf("schedule db1 set: exit=%d", exit)
	}
	// Verify by re-reading.
	stdout, _, exit = runCmd(t, "schedule", "db1", "--output", "json")
	if exit != 0 {
		t.Fatalf("re-read: exit=%d", exit)
	}
	if !strings.Contains(stdout, `"every": "30m"`) {
		t.Errorf("set didn't persist:\n%s", stdout)
	}
}

// TestScheduleList_HelpDiscoverable: the new no-args mode +
// fleet view is documented in --help.
func TestScheduleList_HelpDiscoverable(t *testing.T) {
	stdout, _, _ := runCmd(t, "schedule", "--help")
	for _, want := range []string{
		"fleet view",
		"list every deployment",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("schedule --help missing %q:\n%s", want, stdout)
		}
	}
}
