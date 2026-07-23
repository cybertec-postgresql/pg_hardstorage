package cli

import (
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
)

// The drill schedule registers an agent task like backup/rotate do
// (continuous-restorability probe, integrity program #3).
func TestBuildDrillTask(t *testing.T) {
	dep := config.DeploymentConfig{
		Repo:     "file:///tmp/x",
		Schedule: config.DeploymentSchedule{Drill: config.ScheduleSpec{DailyAt: "03:00"}},
	}
	task, err := buildDrillTask("db1", dep, nil)
	if err != nil {
		t.Fatalf("buildDrillTask: %v", err)
	}
	if task.Name != "drill:db1" {
		t.Errorf("task name = %q, want drill:db1", task.Name)
	}
	// Missing repo must refuse at registration, not at fire time.
	if _, err := buildDrillTask("db1", config.DeploymentConfig{
		Schedule: config.DeploymentSchedule{Drill: config.ScheduleSpec{Every: "24h"}},
	}, nil); err == nil {
		t.Error("missing repo accepted")
	}
	// Bad spec refused.
	if _, err := buildDrillTask("db1", config.DeploymentConfig{
		Repo:     "file:///tmp/x",
		Schedule: config.DeploymentSchedule{Drill: config.ScheduleSpec{Every: "not-a-duration"}},
	}, nil); err == nil {
		t.Error("bad schedule spec accepted")
	}
}
