package cli_test

import (
	stdjson "encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/server"
)

// TestVerify_ControlPlane_HappyPath exercises the new verify
// --control-plane mode end-to-end. A fake "agent" claims and
// completes a JobVerify with a successful sandbox result; the CLI
// sees the success.
func TestVerify_ControlPlane_HappyPath(t *testing.T) {
	args := []string{
		"verify", "db1", "latest",
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
			Op: "verify.sandbox_started",
		})
		if _, err := s.Jobs().Complete(jobID, server.CompleteOptions{
			Success: true,
			Result: map[string]any{
				"deployment":  "db1",
				"backup_id":   "db1.full.20260427T0900Z",
				"pg_major":    "17",
				"image":       "postgres:17",
				"tool":        "pg_verifybackup",
				"passed":      true,
				"duration_ms": 4321,
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
		t.Fatalf("decode: %v\n%s", err, stdout)
	}
	if res.IsError() {
		t.Errorf("Result is error: %+v", res.Error)
	}
}

// TestVerify_ControlPlane_FailedTerminal asserts the CLI maps a
// failed verify to ExitVerifyFailed (9) via the verify.failed code
// prefix.
func TestVerify_ControlPlane_FailedTerminal(t *testing.T) {
	args := []string{
		"verify", "db1", "latest",
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
			Failure: "pg_verifybackup: file size mismatch on global/pg_internal.init",
		}); err != nil {
			t.Errorf("fake agent complete: %v", err)
		}
	}
	stdout, stderr, exit := runCLIWithControlPlane(t, args, onJob)
	if exit != int(output.ExitVerifyFailed) {
		t.Errorf("expected ExitVerifyFailed(%d); got %d\nstderr=%s", output.ExitVerifyFailed, exit, stderr)
	}
	if !strings.Contains(stderr, "verify.failed") {
		t.Errorf("expected verify.failed code; stderr=%s", stderr)
	}
	if !strings.Contains(stderr, "file size mismatch") {
		t.Errorf("expected agent failure message in error; stderr=%s\nstdout=%s", stderr, stdout)
	}
}
