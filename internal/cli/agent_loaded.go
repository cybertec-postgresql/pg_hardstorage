package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"syscall"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/fsutil"
)

// agentLoadedFile is the state-dir record of the schedules the running
// agent loaded at start. The agent builds its task list once, at start;
// editing pg_hardstorage.yaml afterwards (or `schedule <dep> <expr>`) takes
// effect only on restart. `schedule` reads this record so it can say so,
// instead of presenting a configured schedule as if it were running
// (issue #80).
const agentLoadedFile = "agent-loaded-schedule.json"

const agentLoadedSchema = "pg_hardstorage.agent.loaded_schedule.v1"

// agentScheduleTasks are the task slots `schedule` shows.
var agentScheduleTasks = []string{"backup", "rotate", "drill"}

type agentLoadedRecord struct {
	Schema    string    `json:"schema"`
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
	// Schedules is deployment -> task -> the spec the agent registered.
	// A slot absent here is not scheduled in the running agent.
	Schedules map[string]map[string]config.ScheduleSpec `json:"schedules"`
}

// newAgentLoadedRecord snapshots the schedules of the tasks the agent
// actually registered: a slot whose task failed to build (addErrs) is not
// running, so it is left out.
func newAgentLoadedRecord(deps map[string]config.DeploymentConfig, addErrs []agentTaskAddError, now time.Time) *agentLoadedRecord {
	failed := map[string]bool{}
	for _, e := range addErrs {
		failed[e.Deployment+"\x00"+e.Task] = true
	}
	rec := &agentLoadedRecord{
		Schema: agentLoadedSchema, PID: os.Getpid(), StartedAt: now.UTC(),
		Schedules: map[string]map[string]config.ScheduleSpec{},
	}
	for name, dep := range deps {
		for _, task := range agentScheduleTasks {
			spec, err := selectScheduleSpec(dep, task)
			if err != nil || spec.IsZero() || failed[name+"\x00"+task] {
				continue
			}
			if rec.Schedules[name] == nil {
				rec.Schedules[name] = map[string]config.ScheduleSpec{}
			}
			rec.Schedules[name][task] = spec
		}
	}
	return rec
}

// writeAgentLoaded persists rec atomically (tmp + rename + dir fsync) under stateDir.
func writeAgentLoaded(stateDir string, rec *agentLoadedRecord) error {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(stateDir, agentLoadedFile)
	// WriteFileAtomic refuses a stale tmp left by a crash; the record is
	// advisory, so clear it rather than fail the agent's start.
	_ = os.Remove(path + ".tmp")
	return fsutil.WriteFileAtomic(path, b, 0o644)
}

// removeAgentLoaded deletes the record, but only if it is still ours: a
// restart that raced our shutdown has already replaced it.
func removeAgentLoaded(stateDir string) {
	path := filepath.Join(stateDir, agentLoadedFile)
	if rec, err := readAgentLoadedRaw(path); err == nil && rec.PID == os.Getpid() {
		_ = os.Remove(path)
	}
}

func readAgentLoadedRaw(path string) (*agentLoadedRecord, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rec agentLoadedRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, err
	}
	if rec.Schema != agentLoadedSchema || rec.PID <= 0 {
		return nil, errors.New("not an agent loaded-schedule record")
	}
	return &rec, nil
}

// readRunningAgent returns the loaded-schedule record of an agent that is
// still running on this host, or nil. A record left by a crashed agent
// (its pid is gone) is ignored.
func readRunningAgent(stateDir string) *agentLoadedRecord {
	rec, err := readAgentLoadedRaw(filepath.Join(stateDir, agentLoadedFile))
	if err != nil || !pidAlive(rec.PID) {
		return nil
	}
	return rec
}

func pidAlive(pid int) bool {
	if runtime.GOOS == "windows" {
		return true // no signal-0 probe; a clean exit removes the record
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

// scheduleDrift compares the configured schedules against what the agent
// loaded.
type scheduleDrift struct {
	rec *agentLoadedRecord
}

// running returns the spec the agent runs for a slot (zero = not scheduled).
func (d scheduleDrift) running(deployment, task string) config.ScheduleSpec {
	return d.rec.Schedules[deployment][task]
}

// stale reports whether the configured spec differs from the running one.
func (d scheduleDrift) stale(deployment, task string, configured config.ScheduleSpec) bool {
	return d.running(deployment, task) != configured
}

// removedDeployments lists deployments the agent still schedules that the
// configuration no longer has.
func (d scheduleDrift) removedDeployments(cfg *config.Config) []string {
	var out []string
	for name := range d.rec.Schedules {
		if _, ok := cfg.Deployments[name]; !ok {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
