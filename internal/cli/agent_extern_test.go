package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeAgentConfig drops a pg_hardstorage.yaml at the given config dir.
func writeAgentConfig(t *testing.T, configDir, body string) {
	t.Helper()
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "pg_hardstorage.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAgent_DryRun_ListsScheduledTasks(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("PG_HARDSTORAGE_CONFIG_DIR", configDir)

	writeAgentConfig(t, configDir, `
schema: pg_hardstorage.config.v1
deployments:
  db1:
    pg_connection: postgres://backup@host/db
    repo: file:///tmp/repo-not-real
    schedule:
      backup: { every: "6h" }
      rotate: { daily_at: "04:00" }
  db2:
    pg_connection: postgres://backup@host/db
    repo: file:///tmp/repo-not-real
    schedule:
      backup: { every: "12h" }
`)

	out, _, exit := runCmd(t, "agent", "--dry-run", "--output", "json")
	if exit != 0 {
		t.Fatalf("exit = %d\nstdout: %s", exit, out)
	}
	for _, want := range []string{
		`"task_count": 3`, // db1 backup + rotate + db2 backup
		`"backup:db1"`,
		`"rotate:db1"`,
		`"backup:db2"`,
		`"description": "every 6h`,  // 6h0m0s
		`"description": "every 12h`, // 12h0m0s
		`"description": "daily at 04:00`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output:\n%s", want, out)
		}
	}
}

func TestAgent_NoDeployments_ErrorsClearly(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("PG_HARDSTORAGE_CONFIG_DIR", configDir)

	writeAgentConfig(t, configDir, `
schema: pg_hardstorage.config.v1
`)

	_, stderr, exit := runCmd(t, "agent", "--output", "json")
	if exit == 0 {
		t.Fatal("expected non-zero exit when no deployments configured")
	}
	if !strings.Contains(stderr, "no_deployments") && !strings.Contains(stderr, "deployments") {
		t.Errorf("error should mention deployments; got stderr:\n%s", stderr)
	}
}

func TestAgent_DeploymentWithoutSchedule_ErrorsClearly(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("PG_HARDSTORAGE_CONFIG_DIR", configDir)

	writeAgentConfig(t, configDir, `
schema: pg_hardstorage.config.v1
deployments:
  db1:
    pg_connection: postgres://x
    repo: file:///x
`)

	_, stderr, exit := runCmd(t, "agent", "--output", "json")
	if exit == 0 {
		t.Fatal("expected non-zero exit when no schedules declared")
	}
	if !strings.Contains(stderr, "no_tasks") && !strings.Contains(stderr, "schedule") {
		t.Errorf("error should mention missing schedule; got stderr:\n%s", stderr)
	}
}

func TestAgent_SkipsBadDeploymentButRunsRest(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("PG_HARDSTORAGE_CONFIG_DIR", configDir)

	// db1 has a malformed `every`; db2 is healthy. The healthy one
	// should still produce a task; the broken one should appear as
	// a warning event in the stream (we can only see it in the
	// stderr/event channel — JSON Result mode emits one combined doc).
	writeAgentConfig(t, configDir, `
schema: pg_hardstorage.config.v1
deployments:
  db1:
    pg_connection: postgres://x
    repo: file:///x
    schedule:
      backup: { every: "fortnight" }
  db2:
    pg_connection: postgres://x
    repo: file:///x
    schedule:
      backup: { every: "1h" }
`)

	out, _, exit := runCmd(t, "agent", "--dry-run", "--output", "json")
	if exit != 0 {
		t.Fatalf("exit = %d (expected 0 — bad deployment must not block good one)\n%s", exit, out)
	}
	if !strings.Contains(out, `"task_count": 1`) {
		t.Errorf("expected task_count: 1 (only db2); got:\n%s", out)
	}
	if !strings.Contains(out, "backup:db2") {
		t.Errorf("expected backup:db2 in output:\n%s", out)
	}
}

// TestAgent_DryRun_ListsPatroniFollowers: a deployment with the
// new patroni: block surfaces in the dry-run JSON under
// patroni_followers, with the resolved slot name + URL. Defends
// the operator's "what will my agent actually run?" expectation
// against the v0.6+ leader-follow loop.
func TestAgent_DryRun_ListsPatroniFollowers(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("PG_HARDSTORAGE_CONFIG_DIR", configDir)

	writeAgentConfig(t, configDir, `
schema: pg_hardstorage.config.v1
deployments:
  db1:
    pg_connection: postgres://backup@host/db
    repo: file:///tmp/repo-not-real
    schedule:
      backup: { every: "6h" }
    patroni:
      url: http://patroni-leader:8008
      slot: hs_db1_custom
      interval: 3s
  db2:
    pg_connection: postgres://backup@host/db
    repo: file:///tmp/repo-not-real
    schedule:
      backup: { every: "12h" }
    # No patroni block: db2 should not appear in patroni_followers.
`)

	out, _, exit := runCmd(t, "agent", "--dry-run", "--output", "json")
	if exit != 0 {
		t.Fatalf("exit = %d\n%s", exit, out)
	}
	// db1 must appear with its custom slot + URL + interval.
	for _, want := range []string{
		`"patroni_followers"`,
		`"deployment": "db1"`,
		`"url": "http://patroni-leader:8008"`,
		`"slot": "hs_db1_custom"`,
		`"interval": "3s"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in dry-run JSON:\n%s", want, out)
		}
	}
	// db2 must NOT appear.
	if strings.Contains(out, `"deployment": "db2"`) && strings.Contains(out, `patroni_followers`) {
		// Only fail if "db2" appears INSIDE patroni_followers — task
		// list also has db2, so a substring search isn't precise.
		// We approximate by checking that db2 doesn't appear after
		// patroni_followers in the JSON.
		fIdx := strings.Index(out, `"patroni_followers"`)
		if fIdx >= 0 {
			tail := out[fIdx:]
			if strings.Contains(tail, `"deployment": "db2"`) {
				t.Errorf("db2 should not appear in patroni_followers; got:\n%s", out)
			}
		}
	}
}

// TestAgent_DryRun_ListsMultiSlotPatroniFollowers: a v0.6+
// Mechanism 3 dual-slot config surfaces in the dry-run JSON
// under patroni_followers[].slots (not the single-slot
// .slot field). Pin the JSON shape so a regression doesn't
// silently re-route to the legacy single-slot rendering.
func TestAgent_DryRun_ListsMultiSlotPatroniFollowers(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("PG_HARDSTORAGE_CONFIG_DIR", configDir)

	writeAgentConfig(t, configDir, `
schema: pg_hardstorage.config.v1
deployments:
  db1:
    pg_connection: postgres://backup@host/db
    repo: file:///tmp/repo-not-real
    schedule:
      backup: { every: "6h" }
    patroni:
      url: http://patroni-leader:8008
      slots:
        - { name: pg_hardstorage_db1_primary, role: leader }
        - { name: pg_hardstorage_db1_replica, role: replica }
`)

	out, _, exit := runCmd(t, "agent", "--dry-run", "--output", "json")
	if exit != 0 {
		t.Fatalf("exit = %d\n%s", exit, out)
	}
	for _, want := range []string{
		`"patroni_followers"`,
		`"deployment": "db1"`,
		`"name": "pg_hardstorage_db1_primary"`,
		`"role": "leader"`,
		`"name": "pg_hardstorage_db1_replica"`,
		`"role": "replica"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in dry-run JSON:\n%s", want, out)
		}
	}
	// Single-slot legacy field must be absent for multi-slot
	// configs (we use the slots array instead).
	if strings.Contains(out, `"slot": "pg_hardstorage`) {
		t.Errorf("multi-slot config should not emit legacy 'slot' field:\n%s", out)
	}
}

// TestAgent_DryRun_OmitsPatroniWhenNoneConfigured: when no
// deployment opts in, the patroni_followers field is omitted
// (omitempty) so the JSON shape stays clean for the common case.
func TestAgent_DryRun_OmitsPatroniWhenNoneConfigured(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("PG_HARDSTORAGE_CONFIG_DIR", configDir)

	writeAgentConfig(t, configDir, `
schema: pg_hardstorage.config.v1
deployments:
  db1:
    pg_connection: postgres://backup@host/db
    repo: file:///tmp/repo-not-real
    schedule:
      backup: { every: "6h" }
`)

	out, _, exit := runCmd(t, "agent", "--dry-run", "--output", "json")
	if exit != 0 {
		t.Fatalf("exit = %d\n%s", exit, out)
	}
	if strings.Contains(out, `"patroni_followers"`) {
		t.Errorf("patroni_followers should be omitted when no deployment opts in:\n%s", out)
	}
}
