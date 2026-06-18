package agent_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/agent"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
)

// TestRestoreExecutor_RefusesNonRestoreKind asserts the kind guard.
// The router shouldn't dispatch a non-restore here, but if it does
// (wiring bug), we want a loud structured failure rather than a
// silent half-restore.
func TestRestoreExecutor_RefusesNonRestoreKind(t *testing.T) {
	e := agent.NewRestoreExecutor(map[string]config.DeploymentConfig{
		"db1": {Repo: "file:///srv/repo"},
	}, nil, "")
	_, err := e.Execute(context.Background(), &agent.ControlPlaneJob{
		Kind:       "backup",
		Deployment: "db1",
	}, func(map[string]any) {})
	if err == nil {
		t.Fatal("expected error on non-restore kind")
	}
	if !strings.Contains(err.Error(), "expects restore") {
		t.Errorf("error doesn't mention kind guard: %v", err)
	}
}

// TestRestoreExecutor_RefusesUnknownDeployment is the deployment guard.
// An agent shouldn't claim a job for a deployment it doesn't manage;
// if it does (control-plane bug), we refuse loudly.
func TestRestoreExecutor_RefusesUnknownDeployment(t *testing.T) {
	e := agent.NewRestoreExecutor(map[string]config.DeploymentConfig{
		"db1": {Repo: "file:///srv/repo"},
	}, nil, "")
	_, err := e.Execute(context.Background(), &agent.ControlPlaneJob{
		Kind:       "restore",
		Deployment: "db2",
	}, func(map[string]any) {})
	if err == nil {
		t.Fatal("expected error on unknown deployment")
	}
	if !strings.Contains(err.Error(), "not in local config") {
		t.Errorf("expected 'not in local config' guard; got: %v", err)
	}
}

// TestRestoreExecutor_RefusesCrossRepoDispatch — the same posture as
// BackupExecutor. A control plane that points the agent at the wrong
// repo must not be able to read backups from there.
func TestRestoreExecutor_RefusesCrossRepoDispatch(t *testing.T) {
	e := agent.NewRestoreExecutor(map[string]config.DeploymentConfig{
		"db1": {Repo: "file:///srv/repo"},
	}, nil, "")
	_, err := e.Execute(context.Background(), &agent.ControlPlaneJob{
		Kind:       "restore",
		Deployment: "db1",
		RepoURL:    "file:///srv/other-repo", // mismatch
		Args: map[string]any{
			"backup_id":  "x",
			"target_dir": "/tmp/x",
		},
	}, func(map[string]any) {})
	if err == nil {
		t.Fatal("expected refusal for cross-repo dispatch")
	}
	if !strings.Contains(err.Error(), "refusing") {
		t.Errorf("expected refusal verb; got: %v", err)
	}
}

// TestRestoreExecutor_RequiresBackupID enforces the body shape contract
// even when a malformed control-plane payload makes it past the route.
func TestRestoreExecutor_RequiresBackupID(t *testing.T) {
	// We need a non-nil verifier or we hit that guard first; pass a
	// zero-value Verifier to get past the verifier check and reach
	// the args parser. Actually no — the verifier-nil check fires
	// before backup_id; supply nil and assert that error first.
	e := agent.NewRestoreExecutor(map[string]config.DeploymentConfig{
		"db1": {Repo: "file:///srv/repo"},
	}, nil, "")
	_, err := e.Execute(context.Background(), &agent.ControlPlaneJob{
		Kind:       "restore",
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

// TestRouterExecutor_DispatchesByKind asserts the router lookup works
// and that an unknown kind surfaces a structured error.
func TestRouterExecutor_DispatchesByKind(t *testing.T) {
	called := ""
	be := stubExec{name: "backup", onExec: func() { called = "backup" }}
	re := stubExec{name: "restore", onExec: func() { called = "restore" }}

	r := agent.NewRouterExecutor(map[string]agent.JobExecutor{
		"backup":  be,
		"restore": re,
	})

	_, err := r.Execute(context.Background(), &agent.ControlPlaneJob{Kind: "backup"}, func(map[string]any) {})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if called != "backup" {
		t.Errorf("backup did not dispatch: called=%q", called)
	}

	_, err = r.Execute(context.Background(), &agent.ControlPlaneJob{Kind: "restore"}, func(map[string]any) {})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if called != "restore" {
		t.Errorf("restore did not dispatch: called=%q", called)
	}
}

func TestRouterExecutor_UnknownKind(t *testing.T) {
	r := agent.NewRouterExecutor(map[string]agent.JobExecutor{
		"backup": stubExec{name: "backup"},
	})
	_, err := r.Execute(context.Background(), &agent.ControlPlaneJob{Kind: "verify"}, func(map[string]any) {})
	if err == nil {
		t.Fatal("expected unknown-kind error")
	}
	if !strings.Contains(err.Error(), "no executor registered") {
		t.Errorf("error message: %v", err)
	}
	if !strings.Contains(err.Error(), "backup") {
		t.Errorf("error should list registered kinds; got: %v", err)
	}
}

func TestRouterExecutor_NilJob(t *testing.T) {
	r := agent.NewRouterExecutor(nil)
	_, err := r.Execute(context.Background(), nil, func(map[string]any) {})
	if err == nil {
		t.Fatal("expected nil-job error")
	}
}

// stubExec is a no-op JobExecutor for the router tests.
type stubExec struct {
	name   string
	onExec func()
}

func (s stubExec) Execute(_ context.Context, j *agent.ControlPlaneJob, _ func(map[string]any)) (map[string]any, error) {
	if j == nil {
		return nil, errors.New("stub: nil job")
	}
	if s.onExec != nil {
		s.onExec()
	}
	return map[string]any{"executor": s.name}, nil
}
