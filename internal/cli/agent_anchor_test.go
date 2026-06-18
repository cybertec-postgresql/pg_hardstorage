package cli

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/audit"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage/fs"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/repo"
)

func TestBuildAnchorTask_RequiresRepo(t *testing.T) {
	_, err := buildAnchorTask("db1", config.DeploymentConfig{
		Schedule: config.DeploymentSchedule{
			AuditAnchor: config.ScheduleSpec{Every: "30m"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "missing repo") {
		t.Errorf("err = %v, want missing-repo", err)
	}
}

func TestBuildAnchorTask_RejectsBadSchedule(t *testing.T) {
	_, err := buildAnchorTask("db1", config.DeploymentConfig{
		Repo: "file:///tmp",
		Schedule: config.DeploymentSchedule{
			AuditAnchor: config.ScheduleSpec{Every: "fortnight"},
		},
	})
	if err == nil {
		t.Fatal("expected schedule parse error")
	}
}

// TestBuildAnchorTask_RunPostsAnchor exercises the closure the
// scheduler will fire: open a real fs repo, plant an audit event,
// run the task, assert an anchor was written.
func TestBuildAnchorTask_RunPostsAnchor(t *testing.T) {
	repoDir := t.TempDir()
	repoURL := "file://" + repoDir
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatal(err)
	}

	// Plant an event so Anchor has something to anchor.
	sp := openTestStorage(t, repoURL)
	store := audit.NewStore(sp)
	if err := store.Append(context.Background(), &audit.Event{Action: "test.tick"}); err != nil {
		t.Fatal(err)
	}
	sp.Close()

	task, err := buildAnchorTask("db1", config.DeploymentConfig{
		Repo: repoURL,
		Schedule: config.DeploymentSchedule{
			AuditAnchor: config.ScheduleSpec{Every: "30m"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if task.Name != "audit-anchor:db1" {
		t.Errorf("Name = %q", task.Name)
	}

	if err := task.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Assert an anchor landed in the repo.
	sp = openTestStorage(t, repoURL)
	defer sp.Close()
	log := audit.NewStorageBackedLog(sp)
	latest, err := log.LatestAnchor(context.Background())
	if err != nil {
		t.Fatalf("LatestAnchor: %v", err)
	}
	if latest == nil {
		t.Fatal("anchor not written")
	}
	if latest.HeadSequence != 0 {
		t.Errorf("HeadSequence = %d, want 0 (single event)", latest.HeadSequence)
	}
}

// TestBuildAnchorTask_RunOnEmptyChainIsNoError covers the
// best-effort posture: a fresh repo with no events shouldn't fail
// the task — the engine would surface that as a noisy error every
// cycle. The closure swallows the empty-chain field-error and lets
// the next cycle catch up.
func TestBuildAnchorTask_RunOnEmptyChainIsNoError(t *testing.T) {
	repoDir := t.TempDir()
	repoURL := "file://" + repoDir
	if _, err := repo.Init(context.Background(), repo.InitOptions{URL: repoURL}); err != nil {
		t.Fatal(err)
	}

	task, err := buildAnchorTask("db1", config.DeploymentConfig{
		Repo: repoURL,
		Schedule: config.DeploymentSchedule{
			AuditAnchor: config.ScheduleSpec{Every: "30m"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := task.Run(context.Background()); err != nil {
		t.Errorf("empty-chain Run should be a no-error no-op; got %v", err)
	}
}

// openTestStorage is a small helper for the anchor tests — spins up
// the fs storage plugin against an existing repo URL.
func openTestStorage(t *testing.T, repoURL string) storage.StoragePlugin {
	t.Helper()
	sp := &fs.Plugin{}
	u, err := url.Parse(repoURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := sp.Open(context.Background(), storage.StorageConfig{URL: u}); err != nil {
		t.Fatal(err)
	}
	return sp
}

// silence-unused: time imported to mirror the agent_test.go pattern,
// reserved for future cron-cadence assertions.
var _ = time.Second
