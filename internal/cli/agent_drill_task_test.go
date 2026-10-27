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
	task, err := buildDrillTask("db1", dep, config.KMSConfig{}, nil)
	if err != nil {
		t.Fatalf("buildDrillTask: %v", err)
	}
	if task.Name != "drill:db1" {
		t.Errorf("task name = %q, want drill:db1", task.Name)
	}
	// Missing repo must refuse at registration, not at fire time.
	if _, err := buildDrillTask("db1", config.DeploymentConfig{
		Schedule: config.DeploymentSchedule{Drill: config.ScheduleSpec{Every: "24h"}},
	}, config.KMSConfig{}, nil); err == nil {
		t.Error("missing repo accepted")
	}
	// A relative tablespace_mapping_all is refused at registration too.
	if _, err := buildDrillTask("db1", config.DeploymentConfig{
		Repo:     "file:///tmp/x",
		Drill:    config.DrillConfig{TablespaceMappingAll: "relative/dir"},
		Schedule: config.DeploymentSchedule{Drill: config.ScheduleSpec{Every: "24h"}},
	}, config.KMSConfig{}, nil); err == nil {
		t.Error("relative drill.tablespace_mapping_all accepted")
	}
	if _, err := buildDrillTask("db1", config.DeploymentConfig{
		Repo:     "file:///tmp/x",
		Drill:    config.DrillConfig{TablespaceMappingAll: "/var/tmp/drill"},
		Schedule: config.DeploymentSchedule{Drill: config.ScheduleSpec{Every: "24h"}},
	}, config.KMSConfig{}, nil); err != nil {
		t.Errorf("absolute drill.tablespace_mapping_all refused: %v", err)
	}
	// Bad spec refused.
	if _, err := buildDrillTask("db1", config.DeploymentConfig{
		Repo:     "file:///tmp/x",
		Schedule: config.DeploymentSchedule{Drill: config.ScheduleSpec{Every: "not-a-duration"}},
	}, config.KMSConfig{}, nil); err == nil {
		t.Error("bad schedule spec accepted")
	}
}
