package cli

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
)

func TestAgentLoadedRecord_OnlyRegisteredTasks(t *testing.T) {
	deps := map[string]config.DeploymentConfig{
		"db1": {Schedule: config.DeploymentSchedule{
			Backup: config.ScheduleSpec{Every: "6h"},
			Rotate: config.ScheduleSpec{DailyAt: "04:00"},
			Drill:  config.ScheduleSpec{Every: "24h"},
		}},
		"db2": {},
	}
	rec := newAgentLoadedRecord(deps, []agentTaskAddError{{Deployment: "db1", Task: "drill", Err: errors.New("x")}}, time.Now())
	if got := rec.Schedules["db1"]["backup"].Every; got != "6h" {
		t.Errorf("backup = %q", got)
	}
	if _, ok := rec.Schedules["db1"]["drill"]; ok {
		t.Error("a task that failed to register is recorded as running")
	}
	if _, ok := rec.Schedules["db2"]; ok {
		t.Error("deployment without schedules recorded")
	}

	dir := t.TempDir()
	if err := writeAgentLoaded(dir, rec); err != nil {
		t.Fatal(err)
	}
	got := readRunningAgent(dir)
	if got == nil || got.PID != os.Getpid() || got.Schedules["db1"]["rotate"].DailyAt != "04:00" {
		t.Fatalf("round trip: %+v", got)
	}
	removeAgentLoaded(dir)
	if readRunningAgent(dir) != nil {
		t.Error("record not removed by its own agent")
	}
	// Another process' record is left alone.
	rec.PID = os.Getpid() + 1
	_ = writeAgentLoaded(dir, rec)
	removeAgentLoaded(dir)
	if _, err := readAgentLoadedRaw(dir + "/" + agentLoadedFile); err != nil {
		t.Error("removeAgentLoaded deleted another agent's record")
	}
}

func TestParseAgentBackupSettings(t *testing.T) {
	avg, to, err := parseAgentBackupSettings(config.BackupConfig{ChunkSize: "1MiB", Timeout: "3d"})
	if err != nil || avg != 1<<20 || to != 72*time.Hour {
		t.Fatalf("got %d, %v, %v", avg, to, err)
	}
	if avg, to, err = parseAgentBackupSettings(config.BackupConfig{}); err != nil || avg != 0 || to != 0 {
		t.Errorf("empty settings must mean defaults, got %d %v %v", avg, to, err)
	}
	for _, bad := range []config.BackupConfig{
		{ChunkSize: "1KiB"}, {ChunkSize: "64MiB"}, {ChunkSize: "lots"}, {Timeout: "-1h"}, {Timeout: "soon"}, {Timeout: "0s"},
	} {
		if _, _, err := parseAgentBackupSettings(bad); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
	task, err := buildBackupTask("db1", config.DeploymentConfig{
		PGConnection: "postgres://x", Repo: "file:///tmp/x",
		Schedule: config.DeploymentSchedule{Backup: config.ScheduleSpec{Every: "6h"}},
		Backup:   config.BackupConfig{Timeout: "36h"},
	}, config.KMSConfig{}, nil, nil)
	if err != nil || task.Timeout != 36*time.Hour {
		t.Errorf("task timeout = %v, %v; want 36h", task.Timeout, err)
	}
}
