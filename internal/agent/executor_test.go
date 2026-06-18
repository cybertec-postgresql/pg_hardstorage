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
	ex := agent.NewBackupExecutor(map[string]config.DeploymentConfig{}, nil, nil)
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
	ex := agent.NewBackupExecutor(deps, nil, nil)
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
	ex := agent.NewBackupExecutor(nil, nil, nil)
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
