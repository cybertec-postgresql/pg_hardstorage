package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedDeployment writes a minimal pg_hardstorage.yaml with one
// deployment so the schedule subcommand has something to mutate.
func seedDeployment(t *testing.T, configDir, name string) {
	t.Helper()
	body := `schema: pg_hardstorage.config.v1
deployments:
  ` + name + `:
    pg_connection: postgres://x@h/db
    repo: file:///tmp/x
    schedule:
      backup: { every: "6h" }
      rotate: { daily_at: "04:00" }
`
	if err := os.WriteFile(filepath.Join(configDir, "pg_hardstorage.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSchedule_Show_PrintsCurrent(t *testing.T) {
	dir := configDir(t)
	seedDeployment(t, dir, "db1")
	out, _, exit := runCmd(t, "schedule", "db1", "--output", "json")
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	for _, want := range []string{
		`"deployment": "db1"`,
		`"task": "backup"`,
		`"every": "6h"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestSchedule_Set_UpdatesConfig(t *testing.T) {
	dir := configDir(t)
	seedDeployment(t, dir, "db1")

	_, _, exit := runCmd(t, "schedule", "db1", "every 30m", "--output", "json")
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "pg_hardstorage.yaml"))
	if !strings.Contains(string(body), "every: 30m") {
		t.Errorf("config should have every: 30m\n%s", body)
	}
}

func TestSchedule_Set_RotateTask(t *testing.T) {
	dir := configDir(t)
	seedDeployment(t, dir, "db1")

	_, _, exit := runCmd(t,
		"schedule", "db1", "daily_at 02:30",
		"--task", "rotate",
		"--output", "json",
	)
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "pg_hardstorage.yaml"))
	if !strings.Contains(string(body), `daily_at: "02:30"`) && !strings.Contains(string(body), "daily_at: 02:30") {
		t.Errorf("rotate schedule update missing\n%s", body)
	}
}

func TestSchedule_Set_Off_ClearsSpec(t *testing.T) {
	dir := configDir(t)
	seedDeployment(t, dir, "db1")
	_, _, exit := runCmd(t, "schedule", "db1", "off", "--output", "json")
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	out, _, _ := runCmd(t, "schedule", "db1", "--output", "json")
	// After "off", the every field should NOT appear.
	if strings.Contains(out, `"every":`) {
		t.Errorf("every should be cleared after `off`; got:\n%s", out)
	}
}

func TestSchedule_Set_BadExpression_Rejected(t *testing.T) {
	dir := configDir(t)
	seedDeployment(t, dir, "db1")
	_, _, exit := runCmd(t, "schedule", "db1", "every fortnight", "--output", "json")
	if exit != 2 {
		t.Errorf("bad expr should exit 2 (Misuse); got %d", exit)
	}
}

func TestSchedule_NoSuchDeployment(t *testing.T) {
	configDir(t)
	_, _, exit := runCmd(t, "schedule", "ghost", "--output", "json")
	if exit != 6 {
		t.Errorf("missing deployment should exit 6 (NotFound); got %d", exit)
	}
}

func TestSchedule_BadTask_Rejected(t *testing.T) {
	dir := configDir(t)
	seedDeployment(t, dir, "db1")
	_, _, exit := runCmd(t, "schedule", "db1", "every 1h", "--task", "verify", "--output", "json")
	if exit != 2 {
		t.Errorf("unknown --task should exit 2 (Misuse); got %d", exit)
	}
}

// `at <RFC3339>` is documented as a valid one-shot expression. Make
// sure it parses to ScheduleSpec.At and not Every.
func TestSchedule_Set_AtAbsolute(t *testing.T) {
	dir := configDir(t)
	seedDeployment(t, dir, "db1")
	_, _, exit := runCmd(t,
		"schedule", "db1", "at 2099-01-01T00:00:00Z",
		"--output", "json",
	)
	if exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "pg_hardstorage.yaml"))
	got := string(body)
	if strings.Contains(got, "every: at") {
		t.Errorf("`at <ts>` was misparsed into the Every field:\n%s", got)
	}
	if !strings.Contains(got, "2099-01-01T00:00:00Z") {
		t.Errorf("at-instant missing from config:\n%s", got)
	}
}
