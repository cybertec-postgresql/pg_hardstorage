package agent_test

import (
	"context"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/agent"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
)

// TestVerifyExecutor_RefusesNonVerifyKind asserts the kind guard.
// Mirror of restore-executor's identical check; the router shouldn't
// dispatch a non-verify here, but if it does, fail loudly.
func TestVerifyExecutor_RefusesNonVerifyKind(t *testing.T) {
	e := agent.NewVerifyExecutor(map[string]config.DeploymentConfig{
		"db1": {Repo: "file:///srv/repo"},
	}, nil, "")
	_, err := e.Execute(context.Background(), &agent.ControlPlaneJob{
		Kind:       "backup",
		Deployment: "db1",
	}, func(map[string]any) {})
	if err == nil {
		t.Fatal("expected refusal of non-verify kind")
	}
	if !strings.Contains(err.Error(), "expects verify") {
		t.Errorf("error = %v", err)
	}
}

// TestVerifyExecutor_RefusesUnknownDeployment is the deployment guard.
func TestVerifyExecutor_RefusesUnknownDeployment(t *testing.T) {
	e := agent.NewVerifyExecutor(map[string]config.DeploymentConfig{
		"db1": {Repo: "file:///srv/repo"},
	}, nil, "")
	_, err := e.Execute(context.Background(), &agent.ControlPlaneJob{
		Kind:       "verify",
		Deployment: "db2",
	}, func(map[string]any) {})
	if err == nil {
		t.Fatal("expected unknown-deployment refusal")
	}
	if !strings.Contains(err.Error(), "not in local config") {
		t.Errorf("error = %v", err)
	}
}

// TestVerifyExecutor_RefusesCrossRepoDispatch — same posture as backup
// + restore. A control plane redirecting verify to a foreign repo is
// refused before any work happens.
func TestVerifyExecutor_RefusesCrossRepoDispatch(t *testing.T) {
	e := agent.NewVerifyExecutor(map[string]config.DeploymentConfig{
		"db1": {Repo: "file:///srv/repo"},
	}, nil, "")
	_, err := e.Execute(context.Background(), &agent.ControlPlaneJob{
		Kind:       "verify",
		Deployment: "db1",
		RepoURL:    "file:///srv/other-repo",
		Args:       map[string]any{"backup_id": "x"},
	}, func(map[string]any) {})
	if err == nil {
		t.Fatal("expected refusal for cross-repo dispatch")
	}
	if !strings.Contains(err.Error(), "refusing") {
		t.Errorf("error = %v", err)
	}
}

// TestVerifyExecutor_RequiresBackupIDOrVerifier — the verifier-nil
// guard fires before backup_id, but missing backup_id still surfaces
// a structured error when the verifier is wired.
func TestVerifyExecutor_RequiresBackupIDOrVerifier(t *testing.T) {
	e := agent.NewVerifyExecutor(map[string]config.DeploymentConfig{
		"db1": {Repo: "file:///srv/repo"},
	}, nil, "")
	_, err := e.Execute(context.Background(), &agent.ControlPlaneJob{
		Kind:       "verify",
		Deployment: "db1",
		Args:       map[string]any{},
	}, func(map[string]any) {})
	if err == nil {
		t.Fatal("expected error when verifier is nil")
	}
	if !strings.Contains(err.Error(), "verifier not loaded") {
		t.Errorf("expected 'verifier not loaded'; got: %v", err)
	}
}
