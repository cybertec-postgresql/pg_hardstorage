package cli_test

import (
	stdjson "encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeLoadedRecord plants the record a running agent writes at start.
func writeLoadedRecord(t *testing.T, stateDir string, pid int, backupEvery string) {
	t.Helper()
	rec := map[string]any{
		"schema":     "pg_hardstorage.agent.loaded_schedule.v1",
		"pid":        pid,
		"started_at": time.Now().Add(-time.Hour).UTC(),
		"schedules":  map[string]any{"db1": map[string]any{"backup": map[string]string{"every": backupEvery}}},
	}
	b, _ := stdjson.Marshal(rec)
	if err := os.WriteFile(filepath.Join(stateDir, "agent-loaded-schedule.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Issue #80: after the config is edited, `schedule` must not present the new
// schedule as if the running agent had it.
func TestSchedule_FlagsConfigNewerThanRunningAgent(t *testing.T) {
	stateDir := agentRestartEnv(t) // db1 backup every 6h

	// An agent that loaded "every 6h" is running (this very process stands in
	// for it: its pid is alive).
	writeLoadedRecord(t, stateDir, os.Getpid(), "6h")
	out, _, exit := runCmd(t, "schedule", "db1", "-o", "json")
	if exit != 0 {
		t.Fatalf("exit=%d\n%s", exit, out)
	}
	if strings.Contains(out, "pending_restart") {
		t.Errorf("config matches the running agent but a restart is flagged:\n%s", out)
	}

	// Edit the config: the running agent still has 6h.
	if _, _, exit := runCmd(t, "schedule", "db1", "every 1h"); exit != 0 {
		t.Fatalf("schedule set exit=%d", exit)
	}
	out, _, _ = runCmd(t, "schedule", "db1", "-o", "json")
	var show struct {
		Result struct {
			Spec           struct{ Every string } `json:"spec"`
			PendingRestart bool                   `json:"pending_restart"`
			AgentRunning   string                 `json:"agent_running"`
			Agent          struct {
				PID             int  `json:"pid"`
				RestartRequired bool `json:"restart_required"`
			} `json:"agent"`
		} `json:"result"`
	}
	if err := stdjson.Unmarshal([]byte(out), &show); err != nil {
		t.Fatal(err, out)
	}
	r := show.Result
	if r.Spec.Every != "1h" || !r.PendingRestart || !strings.Contains(r.AgentRunning, "6h") ||
		r.Agent.PID != os.Getpid() || !r.Agent.RestartRequired {
		t.Errorf("show does not flag the pending restart: %+v\n%s", r, out)
	}
	text, _, _ := runCmd(t, "schedule", "db1", "-o", "text")
	if !strings.Contains(text, "NOT RUNNING YET") || !strings.Contains(text, "restart") {
		t.Errorf("text view hides the pending restart:\n%s", text)
	}

	// Fleet view: the row and the footer both say so.
	list, _, _ := runCmd(t, "schedule", "-o", "text")
	if !strings.Contains(list, "NOT RUNNING YET") || !strings.Contains(list, fmt.Sprintf("pid %d", os.Getpid())) {
		t.Errorf("fleet view hides the pending restart:\n%s", list)
	}
	ljson, _, _ := runCmd(t, "schedule", "-o", "json")
	if !strings.Contains(ljson, `"restart_required": true`) || !strings.Contains(ljson, `"pending_restart": true`) {
		t.Errorf("fleet JSON hides the pending restart:\n%s", ljson)
	}

	// `schedule set` says the same while the agent runs.
	setOut, _, _ := runCmd(t, "schedule", "db1", "every 2h", "-o", "text")
	if !strings.Contains(setOut, "NOT RUNNING YET") {
		t.Errorf("schedule set while an agent runs does not warn:\n%s", setOut)
	}

	// A record left by a crashed agent (pid gone) is ignored.
	writeLoadedRecord(t, stateDir, 0x3ffffffe, "6h")
	out, _, _ = runCmd(t, "schedule", "db1", "-o", "json")
	if strings.Contains(out, "pending_restart") || strings.Contains(out, `"agent"`) {
		t.Errorf("stale record from a dead pid was trusted:\n%s", out)
	}
}

func TestSchedule_SetWithoutRunningAgentSaysWhenItApplies(t *testing.T) {
	agentRestartEnv(t)
	out, _, exit := runCmd(t, "schedule", "db1", "every 2h", "-o", "text")
	if exit != 0 || !strings.Contains(out, "re)starts") {
		t.Errorf("exit=%d, no apply-time note:\n%s", exit, out)
	}
}
