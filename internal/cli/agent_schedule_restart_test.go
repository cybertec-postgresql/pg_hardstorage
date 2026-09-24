package cli_test

import (
	stdjson "encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// agentRestartEnv isolates config, state and keyring dirs and writes a
// config with one `every 6h` backup task.
func agentRestartEnv(t *testing.T) (stateDir string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	configDir := t.TempDir()
	stateDir = t.TempDir()
	t.Setenv("PG_HARDSTORAGE_CONFIG_DIR", configDir)
	t.Setenv("PG_HARDSTORAGE_STATE_DIR", stateDir)
	t.Setenv("PG_HARDSTORAGE_KEYRING_DIR", t.TempDir())
	writeAgentConfig(t, configDir, `
schema: pg_hardstorage.config.v1
deployments:
  db1:
    pg_connection: postgres://backup@host/db
    repo: file:///tmp/repo-not-real
    schedule:
      backup: { every: "6h" }
`)
	return stateDir
}

func agentDryRunNextDue(t *testing.T, task string) time.Time {
	t.Helper()
	out, _, exit := runCmd(t, "agent", "--dry-run", "--output", "json")
	if exit != 0 {
		t.Fatalf("exit = %d\n%s", exit, out)
	}
	var res struct {
		Result struct {
			Tasks []struct {
				Name    string    `json:"name"`
				NextDue time.Time `json:"next_due"`
			} `json:"tasks"`
		} `json:"result"`
	}
	if err := stdjson.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	for _, ts := range res.Result.Tasks {
		if ts.Name == task {
			return ts.NextDue
		}
	}
	t.Fatalf("task %q not in dry-run output:\n%s", task, out)
	return time.Time{}
}

// TestAgent_EveryTask_DueOnFirstStart pins the documented contract
// ("restart the agent and every task is immediately due") for an
// `every` task that has never run. It used to be scheduled a full
// interval out on every start, so an agent restarted more often than
// the interval never took a backup at all.
func TestAgent_EveryTask_DueOnFirstStart(t *testing.T) {
	agentRestartEnv(t)
	before := time.Now()
	due := agentDryRunNextDue(t, "backup:db1")
	if due.After(before.Add(time.Minute)) {
		t.Errorf("next_due = %v, want ~now (%v): a never-run every-task must be due on start", due, before)
	}
}

// TestAgent_EveryTask_ResumesCadenceFromLastRun pins that a restart
// resumes from the persisted last run instead of restarting the
// interval: last run 1h ago on an `every 6h` task → next due 5h from
// now, not 6h.
func TestAgent_EveryTask_ResumesCadenceFromLastRun(t *testing.T) {
	stateDir := agentRestartEnv(t)
	last := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	body := `{"schema":"pg_hardstorage.schedule.lastrun.v1","tasks":{"backup:db1":"` +
		last.Format(time.RFC3339) + `"}}`
	if err := os.WriteFile(filepath.Join(stateDir, "agent-schedule.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	due := agentDryRunNextDue(t, "backup:db1")
	want := last.Add(6 * time.Hour)
	if !due.Equal(want) {
		t.Errorf("next_due = %v, want %v (last run + interval)", due, want)
	}
}
