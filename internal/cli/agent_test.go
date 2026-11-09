package cli

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

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
	p, err := buildRetentionPolicy(config.RetentionConfig{Policy: "count", KeepFulls: config.Keep(21)})
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
			_, err := buildBackupTask("db1", c.dep, config.KMSConfig{}, nil, nil)
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
	}, config.KMSConfig{}, nil, nil)
	if err == nil {
		t.Fatal("expected schedule parse error")
	}
}

func TestKeepOrDefault(t *testing.T) {
	if keepOrDefault(nil, 5) != 5 {
		t.Error("unset should fall to default")
	}
	if keepOrDefault(config.Keep(7), 5) != 7 {
		t.Error("a set count should win")
	}
	if keepOrDefault(config.Keep(0), 5) != 0 {
		t.Error("an explicit 0 is a value, not unset")
	}
}

// An explicit zero in the config disables that bucket, exactly as
// `rotate --keep-daily 0` does; it used to be replaced by the default,
// so the scheduled rotate kept 7 dailies the operator had switched off.
func TestBuildRetentionPolicy_ExplicitZeroIsHonoured(t *testing.T) {
	p, err := buildRetentionPolicy(config.RetentionConfig{
		KeepDaily: config.Keep(0), KeepWeekly: config.Keep(0), KeepMonthly: config.Keep(6),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := retention.GFSPolicy{KeepDaily: 0, KeepWeekly: 0, KeepMonthly: 6, KeepYearly: 5}
	if p != retention.Policy(want) {
		t.Errorf("gfs = %+v, want %+v", p, want)
	}
	p, err = buildRetentionPolicy(config.RetentionConfig{Policy: "count", KeepFulls: config.Keep(0)})
	if err != nil {
		t.Fatal(err)
	}
	if p != retention.Policy(retention.CountPolicy{KeepFulls: 0}) {
		t.Errorf("count = %+v, want KeepFulls 0", p)
	}
}

// The flag path refuses a negative count; so does the config path.
func TestBuildRetentionPolicy_RejectsNegative(t *testing.T) {
	if _, err := buildRetentionPolicy(config.RetentionConfig{KeepWeekly: config.Keep(-1)}); err == nil {
		t.Fatal("keep_weekly: -1 accepted")
	}
}

// The YAML a config file carries: an absent key stays unset, `0` is 0.
func TestRetentionConfig_YAMLDistinguishesUnsetFromZero(t *testing.T) {
	var r config.RetentionConfig
	if err := yaml.Unmarshal([]byte("keep_daily: 0\nkeep_monthly: 3\n"), &r); err != nil {
		t.Fatal(err)
	}
	if r.KeepDaily == nil || *r.KeepDaily != 0 || r.KeepWeekly != nil || r.KeepMonthly == nil || *r.KeepMonthly != 3 {
		t.Fatalf("decoded %+v", r)
	}
	out, err := yaml.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "keep_daily: 0\nkeep_monthly: 3\n" {
		t.Fatalf("re-encoded as %q: an explicit 0 must survive a config edit", out)
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
