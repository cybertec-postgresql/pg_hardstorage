package cli

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/recovery"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

// drillDoctorWorld plants a repo with optional drill-history entries and
// returns a LoadResult wired to it.
func drillDoctorWorld(t *testing.T, entries ...*recovery.DrillHistoryEntry) (*config.LoadResult, string) {
	t.Helper()
	dir := t.TempDir()
	repoURL := "file://" + dir
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatalf("repo init: %v", err)
	}
	sp := &fs.Plugin{}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: &url.URL{Scheme: "file", Path: dir}}); err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	hs := recovery.NewHistoryStore(sp)
	for _, e := range entries {
		if err := hs.Append(context.Background(), e); err != nil {
			t.Fatalf("plant drill entry: %v", err)
		}
	}
	cfg := &config.LoadResult{}
	cfg.Config.Schema = config.Schema
	cfg.Config.Deployments = map[string]config.DeploymentConfig{
		"db1": {Repo: repoURL},
	}
	return cfg, repoURL
}

func drillEntry(id string, verdict recovery.DrillVerdict, at time.Time) *recovery.DrillHistoryEntry {
	return &recovery.DrillHistoryEntry{
		ID:          id,
		Deployment:  "db1",
		BackupID:    "db1.full.x",
		Verdict:     verdict,
		GeneratedAt: at,
		StoppedAt:   at.Add(time.Minute),
	}
}

func issueCodes(issues []doctorIssue) string {
	var b strings.Builder
	for _, i := range issues {
		b.WriteString(i.Code + " ")
	}
	return b.String()
}

// The continuous-restorability probe (concurrency/integrity program #3):
// doctor must surface never-run (notice), failing (CRITICAL), stale
// (CRITICAL), and stay quiet when the last pass is fresh.
func TestAppendDrillChecks_Verdicts(t *testing.T) {
	now := time.Now().UTC()

	t.Run("never_run_notice", func(t *testing.T) {
		cfg, _ := drillDoctorWorld(t)
		_, issues := appendDrillChecks(context.Background(), cfg, 0, nil)
		if !strings.Contains(issueCodes(issues), "recovery.drill_never_run") {
			t.Errorf("want drill_never_run; got %q", issueCodes(issues))
		}
	})

	t.Run("fresh_pass_is_quiet", func(t *testing.T) {
		cfg, _ := drillDoctorWorld(t, drillEntry("2000000000-db1-aaaa", recovery.DrillVerdictPass, now.Add(-2*time.Hour)))
		reps, issues := appendDrillChecks(context.Background(), cfg, 0, nil)
		if len(issues) != 0 {
			t.Errorf("fresh pass produced issues: %q", issueCodes(issues))
		}
		if len(reps) != 1 || !reps[0].Fresh {
			t.Errorf("report = %+v, want Fresh=true", reps)
		}
	})

	t.Run("stale_pass_is_critical", func(t *testing.T) {
		cfg, _ := drillDoctorWorld(t, drillEntry("1000000000-db1-aaaa", recovery.DrillVerdictPass, now.Add(-30*24*time.Hour)))
		_, issues := appendDrillChecks(context.Background(), cfg, 7*24*time.Hour, nil)
		if !strings.Contains(issueCodes(issues), "recovery.drill_stale") {
			t.Fatalf("want drill_stale; got %q", issueCodes(issues))
		}
	})

	t.Run("latest_fail_is_critical", func(t *testing.T) {
		cfg, _ := drillDoctorWorld(t,
			drillEntry("1000000000-db1-aaaa", recovery.DrillVerdictPass, now.Add(-3*time.Hour)),
			drillEntry("2000000000-db1-bbbb", recovery.DrillVerdictFail, now.Add(-1*time.Hour)),
		)
		_, issues := appendDrillChecks(context.Background(), cfg, 0, nil)
		if !strings.Contains(issueCodes(issues), "recovery.drill_failing") {
			t.Fatalf("want drill_failing; got %q", issueCodes(issues))
		}
	})
}
