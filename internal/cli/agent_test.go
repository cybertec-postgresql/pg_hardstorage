package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup/retention"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/schedule"
)

func TestBuildRetentionPolicy_DefaultsToGFS(t *testing.T) {
	p, err := buildRetentionPolicy(config.RetentionConfig{})
	if err != nil {
		t.Fatal(err)
	}
	gfs, ok := p.(retention.GFSPolicy)
	if !ok {
		t.Fatalf("got %T, want GFSPolicy", p)
	}
	if gfs.KeepDaily != 7 || gfs.KeepWeekly != 4 || gfs.KeepMonthly != 12 || gfs.KeepYearly != 5 {
		t.Errorf("default GFS numbers wrong: %+v", gfs)
	}
}

func TestBuildRetentionPolicy_SimpleHonoursKeepFor(t *testing.T) {
	p, err := buildRetentionPolicy(config.RetentionConfig{Policy: "simple", KeepFor: "12h"})
	if err != nil {
		t.Fatal(err)
	}
	s, ok := p.(retention.SimplePolicy)
	if !ok {
		t.Fatalf("got %T, want SimplePolicy", p)
	}
	if s.KeepFor != 12*time.Hour {
		t.Errorf("KeepFor = %v, want 12h", s.KeepFor)
	}
}

func TestBuildRetentionPolicy_SimpleDefaultIs30d(t *testing.T) {
	p, _ := buildRetentionPolicy(config.RetentionConfig{Policy: "simple"})
	s := p.(retention.SimplePolicy)
	if s.KeepFor != 30*24*time.Hour {
		t.Errorf("default KeepFor = %v, want 30d", s.KeepFor)
	}
}

func TestBuildRetentionPolicy_CountHonoursKeepFulls(t *testing.T) {
	p, err := buildRetentionPolicy(config.RetentionConfig{Policy: "count", KeepFulls: 21})
	if err != nil {
		t.Fatal(err)
	}
	c, ok := p.(retention.CountPolicy)
	if !ok {
		t.Fatalf("got %T, want CountPolicy", p)
	}
	if c.KeepFulls != 21 {
		t.Errorf("KeepFulls = %d, want 21", c.KeepFulls)
	}
}

func TestBuildRetentionPolicy_RejectsUnknown(t *testing.T) {
	_, err := buildRetentionPolicy(config.RetentionConfig{Policy: "fortnight"})
	if err == nil {
		t.Fatal("expected unknown-policy error")
	}
	if !strings.Contains(err.Error(), "fortnight") {
		t.Errorf("error should name the policy; got %v", err)
	}
}

func TestBuildRetentionPolicy_RejectsBadKeepFor(t *testing.T) {
	_, err := buildRetentionPolicy(config.RetentionConfig{Policy: "simple", KeepFor: "two weeks"})
	if err == nil {
		t.Fatal("expected duration parse error")
	}
}

func TestBuildBackupTask_RequiresFields(t *testing.T) {
	cases := []struct {
		name string
		dep  config.DeploymentConfig
		want string
	}{
		{"no pg_connection", config.DeploymentConfig{Repo: "file://x", Schedule: config.DeploymentSchedule{Backup: config.ScheduleSpec{Every: "1h"}}}, "pg_connection"},
		{"no repo", config.DeploymentConfig{PGConnection: "postgres://", Schedule: config.DeploymentSchedule{Backup: config.ScheduleSpec{Every: "1h"}}}, "repo"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := buildBackupTask("db1", c.dep, nil, nil)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want one mentioning %q", err, c.want)
			}
		})
	}
}

func TestBuildBackupTask_RejectsBadSchedule(t *testing.T) {
	_, err := buildBackupTask("db1", config.DeploymentConfig{
		PGConnection: "postgres://",
		Repo:         "file://x",
		Schedule:     config.DeploymentSchedule{Backup: config.ScheduleSpec{Every: "fortnight"}},
	}, nil, nil)
	if err == nil {
		t.Fatal("expected schedule parse error")
	}
}

func TestDefaultIfZero(t *testing.T) {
	if defaultIfZero(0, 5) != 5 {
		t.Error("zero should fall to default")
	}
	if defaultIfZero(7, 5) != 7 {
		t.Error("non-zero should win")
	}
}

func TestDryRunBody_WriteText(t *testing.T) {
	body := dryRunBody{
		TaskCount: 2,
		Tasks: []schedule.TaskStatus{
			{Name: "backup:db1", Description: "every 6h", NextDue: time.Date(2026, 4, 28, 18, 0, 0, 0, time.UTC)},
			{Name: "rotate:db1", Description: "daily at 04:00", NextDue: time.Date(2026, 4, 29, 4, 0, 0, 0, time.UTC)},
		},
	}
	var sb strings.Builder
	if err := body.WriteText(&sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, want := range []string{
		"Agent dry-run: 2 task(s)",
		"backup:db1",
		"rotate:db1",
		"every 6h",
		"daily at 04:00",
		"2026-04-28T18:00:00Z",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\ngot:\n%s", want, out)
		}
	}
}
