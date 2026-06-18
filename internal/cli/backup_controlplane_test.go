package cli_test

import (
	"bytes"
	stdjson "encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/cli"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/server"
)

// TestBackup_ControlPlane_HappyPath drives the new backup
// --control-plane mode through to a successful terminal state. Same
// shape as the restore happy-path test: a fake "agent" goroutine
// claims → progresses → completes, the CLI sees the success.
func TestBackup_ControlPlane_HappyPath(t *testing.T) {
	args := []string{
		"backup", "db1",
		"-o", "json",
	}
	onJob := func(s *server.Server, jobID string) {
		if _, err := s.Jobs().Claim(server.ClaimOptions{
			AgentID:     "fake-agent",
			Deployments: []string{"db1"},
		}); err != nil {
			t.Errorf("fake agent claim: %v", err)
			return
		}
		_ = s.Jobs().AppendProgress(jobID, server.ProgressEvent{
			At: time.Now().UTC(),
			Op: "backup.progress",
			Body: map[string]any{
				"bytes_logical": 4096,
			},
		})
		if _, err := s.Jobs().Complete(jobID, server.CompleteOptions{
			Success: true,
			Result: map[string]any{
				"backup_id":          "db1.full.20260429T1500Z",
				"deployment":         "db1",
				"unique_chunk_count": 128,
				"logical_bytes":      8192,
				"duration_ms":        1234,
			},
		}); err != nil {
			t.Errorf("fake agent complete: %v", err)
		}
	}
	stdout, stderr, exit := runCLIWithControlPlane(t, args, onJob)
	if exit != int(output.ExitOK) {
		t.Fatalf("exit=%d (want 0)\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}
	var res output.Result
	if err := stdjson.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("decode Result: %v\n%s", err, stdout)
	}
	if res.IsError() {
		t.Errorf("Result is error: %+v", res.Error)
	}
}

// TestBackup_ControlPlane_FailedTerminal asserts the CLI maps a
// failed backup job to a non-zero exit + structured error code.
func TestBackup_ControlPlane_FailedTerminal(t *testing.T) {
	args := []string{
		"backup", "db1",
		"-o", "json",
	}
	onJob := func(s *server.Server, jobID string) {
		if _, err := s.Jobs().Claim(server.ClaimOptions{
			AgentID:     "fake-agent",
			Deployments: []string{"db1"},
		}); err != nil {
			t.Errorf("fake agent claim: %v", err)
			return
		}
		if _, err := s.Jobs().Complete(jobID, server.CompleteOptions{
			Success: false,
			Failure: "BASE_BACKUP disconnected after 47s",
		}); err != nil {
			t.Errorf("fake agent complete: %v", err)
		}
	}
	stdout, stderr, exit := runCLIWithControlPlane(t, args, onJob)
	if exit == int(output.ExitOK) {
		t.Fatalf("expected non-zero exit on failed job; stdout=%s stderr=%s", stdout, stderr)
	}
	if !strings.Contains(stderr, "backup.failed") {
		t.Errorf("expected backup.failed code; stderr=%s", stderr)
	}
	if !strings.Contains(stderr, "BASE_BACKUP disconnected") {
		t.Errorf("expected agent failure message in error; stderr=%s", stderr)
	}
}

// TestBackup_ControlPlane_RefusesTenantFlag asserts the local
// pre-flight refuses --tenant in control-plane mode (it's an
// agent-side concern).
func TestBackup_ControlPlane_RefusesTenantFlag(t *testing.T) {
	root := cli.NewRoot()
	var out, errb bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetArgs([]string{
		"backup", "db1",
		"--tenant", "acme-prod",
		"--control-plane", "http://does-not-matter:8443",
		"-o", "json",
	})
	exit := cli.Run(root)
	if exit != int(output.ExitMisuse) {
		t.Errorf("expected ExitMisuse(%d); got %d\n%s", output.ExitMisuse, exit, errb.String())
	}
	if !strings.Contains(errb.String(), "usage.unsupported_flag") {
		t.Errorf("expected usage.unsupported_flag code; stderr=%s", errb.String())
	}
}

// TestBackup_ControlPlane_RefusesEncryptFlag asserts the local
// pre-flight refuses --encrypt / --no-encrypt in control-plane mode
// (the agent's keyring picks the posture).
func TestBackup_ControlPlane_RefusesEncryptFlag(t *testing.T) {
	root := cli.NewRoot()
	var out, errb bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetArgs([]string{
		"backup", "db1",
		"--encrypt",
		"--control-plane", "http://does-not-matter:8443",
		"-o", "json",
	})
	exit := cli.Run(root)
	if exit != int(output.ExitMisuse) {
		t.Errorf("expected ExitMisuse(%d); got %d\n%s", output.ExitMisuse, exit, errb.String())
	}
	if !strings.Contains(errb.String(), "usage.unsupported_flag") {
		t.Errorf("expected usage.unsupported_flag code; stderr=%s", errb.String())
	}
}

// TestBackup_ControlPlane_FastFlowsThroughArgs asserts the --fast
// flag rides into Job.Args so the agent's BackupExecutor sees it.
func TestBackup_ControlPlane_FastFlowsThroughArgs(t *testing.T) {
	args := []string{
		"backup", "db1",
		"--fast",
		"--label", "before-migration",
		"-o", "json",
	}
	onJob := func(s *server.Server, jobID string) {
		// Inspect the job before claiming. The Args we sent should
		// be present.
		j, err := s.Jobs().Get(jobID)
		if err != nil {
			t.Errorf("Get job: %v", err)
			return
		}
		if j.Args["fast"] != true {
			t.Errorf("Args.fast = %v, want true", j.Args["fast"])
		}
		if j.Args["label"] != "before-migration" {
			t.Errorf("Args.label = %v", j.Args["label"])
		}
		// Then drive the job to completion so the test exits.
		if _, err := s.Jobs().Claim(server.ClaimOptions{
			AgentID:     "a",
			Deployments: []string{"db1"},
		}); err != nil {
			t.Errorf("claim: %v", err)
			return
		}
		if _, err := s.Jobs().Complete(jobID, server.CompleteOptions{
			Success: true,
			Result:  map[string]any{"backup_id": "x"},
		}); err != nil {
			t.Errorf("complete: %v", err)
		}
	}
	stdout, stderr, exit := runCLIWithControlPlane(t, args, onJob)
	if exit != int(output.ExitOK) {
		t.Fatalf("exit=%d\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}
}
