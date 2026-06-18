package cli_test

import (
	"bytes"
	stdjson "encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/cli"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/server"
)

// runCLIWithControlPlane is a tiny helper that drives the cli.Root
// against a live in-memory control plane and returns stdout/stderr/exit.
// The server is wrapped in a goroutine that observes new jobs and
// completes them with synthetic results, so the CLI's poll loop sees
// progress events + a terminal state without us standing up a real
// agent.
func runCLIWithControlPlane(t *testing.T, args []string, onJob func(s *server.Server, jobID string)) (stdout, stderr string, exit int) {
	t.Helper()
	s, err := server.New(server.Config{Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(s.Handler())
	t.Cleanup(hs.Close)

	// Watch for new jobs on a goroutine. When the test's onJob
	// callback fires, it can post progress + complete the job
	// against the live server.jobs API.
	done := make(chan struct{})
	go func() {
		defer close(done)
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			jobs := s.Jobs().List(server.ListOptions{State: server.JobQueued})
			if len(jobs) > 0 && onJob != nil {
				onJob(s, jobs[0].ID)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	root := cli.NewRoot()
	var out, errb bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetArgs(append(args, "--control-plane", hs.URL, "--control-plane-poll-secs", "0"))
	exit = cli.Run(root)
	<-done
	return out.String(), errb.String(), exit
}

// TestRestore_ControlPlane_HappyPath drives the new --control-plane
// path through to a successful terminal state. We dispatch a restore,
// have a fake "agent" claim → progress → complete, and assert the
// CLI sees the success.
func TestRestore_ControlPlane_HappyPath(t *testing.T) {
	args := []string{
		"restore", "db1", "latest",
		"--target", "/tmp/restored",
		"-o", "json",
	}
	onJob := func(s *server.Server, jobID string) {
		// Claim — emulates what an agent would do.
		if _, err := s.Jobs().Claim(server.ClaimOptions{
			AgentID:     "fake-agent",
			Deployments: []string{"db1"},
		}); err != nil {
			t.Errorf("fake agent claim: %v", err)
			return
		}
		// One progress event, then complete.
		_ = s.Jobs().AppendProgress(jobID, server.ProgressEvent{
			At: time.Now().UTC(),
			Op: "restore.progress",
			Body: map[string]any{
				"bytes_written": 1024,
			},
		})
		if _, err := s.Jobs().Complete(jobID, server.CompleteOptions{
			Success: true,
			Result: map[string]any{
				"backup_id":     "db1.full.20260427T0900Z",
				"target_dir":    "/tmp/restored",
				"file_count":    42,
				"bytes_written": 4096,
			},
		}); err != nil {
			t.Errorf("fake agent complete: %v", err)
		}
	}
	stdout, stderr, exit := runCLIWithControlPlane(t, args, onJob)
	if exit != int(output.ExitOK) {
		t.Fatalf("exit=%d (want 0)\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}
	if stdout == "" {
		t.Fatalf("expected JSON Result on stdout; stderr=%s", stderr)
	}
	var res output.Result
	if err := stdjson.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("decode Result: %v\n%s", err, stdout)
	}
	if res.IsError() {
		t.Errorf("Result is error: %+v", res.Error)
	}
}

// TestRestore_ControlPlane_FailedTerminal asserts the CLI maps a
// failed job to a non-zero exit + a structured error code.
func TestRestore_ControlPlane_FailedTerminal(t *testing.T) {
	args := []string{
		"restore", "db1", "latest",
		"--target", "/tmp/restored",
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
			Failure: "chunker exhausted retries",
		}); err != nil {
			t.Errorf("fake agent complete: %v", err)
		}
	}
	stdout, stderr, exit := runCLIWithControlPlane(t, args, onJob)
	if exit == int(output.ExitOK) {
		t.Fatalf("expected non-zero exit on failed job; stdout=%s stderr=%s", stdout, stderr)
	}
	// In JSON mode, errors land on stderr.
	if !strings.Contains(stderr, "restore.failed") {
		t.Errorf("expected restore.failed code; stderr=%s", stderr)
	}
	if !strings.Contains(stderr, "chunker exhausted retries") {
		t.Errorf("expected agent failure message in error; stderr=%s", stderr)
	}
}

// TestRestore_ControlPlane_MissingTarget asserts the CLI's local
// pre-flight refuses --control-plane without --target before any
// network round-trip. Operator gets a clear local error rather than
// a 400 from the server.
func TestRestore_ControlPlane_MissingTarget(t *testing.T) {
	root := cli.NewRoot()
	var out, errb bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetArgs([]string{
		"restore", "db1", "latest",
		"--control-plane", "http://does-not-matter:8443",
		"-o", "json",
	})
	exit := cli.Run(root)
	if exit != int(output.ExitMisuse) {
		t.Errorf("expected ExitMisuse(%d); got %d\n%s", output.ExitMisuse, exit, errb.String())
	}
	if !strings.Contains(errb.String(), "--target is required") {
		t.Errorf("expected target-required message; stderr=%s", errb.String())
	}
}

// TestRestore_ControlPlane_ConflictingTargets asserts the local
// one-target rule fires before the network round-trip.
func TestRestore_ControlPlane_ConflictingTargets(t *testing.T) {
	root := cli.NewRoot()
	var out, errb bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetArgs([]string{
		"restore", "db1", "latest",
		"--control-plane", "http://does-not-matter:8443",
		"--target", "/tmp/x",
		"--to", "5 minutes ago",
		"--to-lsn", "0/3000028",
		"-o", "json",
	})
	exit := cli.Run(root)
	if exit != int(output.ExitMisuse) {
		t.Errorf("expected ExitMisuse(%d); got %d\n%s", output.ExitMisuse, exit, errb.String())
	}
	if !strings.Contains(errb.String(), "conflicting_targets") {
		t.Errorf("expected conflicting_targets code; stderr=%s", errb.String())
	}
}
