package cli_test

import (
	"bytes"
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/cli"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/server"
)

// runCPNoAgent drives the CLI against a live in-memory control plane
// with NO agent: nothing ever claims the job. It returns the exit code,
// combined output and the server so the test can inspect what (if
// anything) was enqueued. ctx bounds the run.
func runCPNoAgent(t *testing.T, ctx context.Context, args []string) (int, string, *server.Server) {
	t.Helper()
	s, err := server.New(server.Config{Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(s.Handler())
	t.Cleanup(hs.Close)
	root := cli.NewRoot()
	var out, errb bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetContext(ctx)
	root.SetArgs(append(args, "--control-plane", hs.URL, "--control-plane-poll-secs", "0"))
	exit := cli.Run(root)
	return exit, out.String() + errb.String(), s
}

func allJobs(t *testing.T, s *server.Server) []server.Job {
	t.Helper()
	jobs, err := s.Jobs().List(server.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return jobs
}

// TestRestore_ControlPlane_PreviewRefused pins C2: --preview promises
// "plan the restore but do not write anything", but the control-plane
// dispatch returned before the preview branch and enqueued a REAL
// restore. It must be refused as a usage error with nothing enqueued.
func TestRestore_ControlPlane_PreviewRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	exit, out, s := runCPNoAgent(t, ctx, []string{
		"restore", "db1", "latest", "--target", "/tmp/restored", "--force", "--preview", "-o", "json",
	})
	if ctx.Err() != nil {
		t.Fatal("restore --preview --control-plane was dispatched and polled until the deadline")
	}
	if exit != int(output.ExitMisuse) {
		t.Errorf("exit = %d, want %d (usage)\n%s", exit, output.ExitMisuse, out)
	}
	if n := len(allJobs(t, s)); n != 0 {
		t.Errorf("%d job(s) enqueued by a --preview run; want 0", n)
	}
}

// TestRestore_ControlPlane_UnsupportedFlagsRefused pins H1's refusal
// half: flags the agent cannot honour must fail loudly (usage error,
// nothing enqueued), never be silently dropped.
func TestRestore_ControlPlane_UnsupportedFlagsRefused(t *testing.T) {
	for name, extra := range map[string][]string{
		"require-threshold-attestation": {"--require-threshold-attestation", "roster-1"},
		"verify-restore":                {"--verify-restore", "required"},
		"kms-config":                    {"--kms-config", "region=eu-central-1"},
		"chain-staging-root":            {"--chain-staging-root", "/var/tmp/stage"},
		"reset-chain-staging":           {"--reset-chain-staging"},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			args := append([]string{"restore", "db1", "latest", "--target", "/tmp/restored", "-o", "json"}, extra...)
			exit, out, s := runCPNoAgent(t, ctx, args)
			if ctx.Err() != nil {
				t.Fatalf("--%s was silently dropped: the restore was dispatched", name)
			}
			if exit != int(output.ExitMisuse) {
				t.Errorf("exit = %d, want %d\n%s", exit, output.ExitMisuse, out)
			}
			if !strings.Contains(out, "usage.unsupported_flag") {
				t.Errorf("want usage.unsupported_flag in output:\n%s", out)
			}
			if n := len(allJobs(t, s)); n != 0 {
				t.Errorf("%d job(s) enqueued; want 0", n)
			}
		})
	}
}

// TestRestore_ControlPlane_ForwardsSupportedFlags pins H1's forwarding
// half: --to-latest, --skip-gap-check, --force-foreign and an explicit
// --verify reach the job args the agent's RestoreExecutor reads.
func TestRestore_ControlPlane_ForwardsSupportedFlags(t *testing.T) {
	var got map[string]any
	onJob := func(s *server.Server, jobID string) {
		j, err := s.Jobs().Get(jobID)
		if err != nil {
			t.Error(err)
			return
		}
		got = j.Args
		if _, err := s.Jobs().Claim(server.ClaimOptions{AgentID: "fake", Deployments: []string{"db1"}}); err != nil {
			t.Error(err)
			return
		}
		if _, err := s.Jobs().Complete(jobID, server.CompleteOptions{Success: true}); err != nil {
			t.Error(err)
		}
	}
	_, stderr, exit := runCLIWithControlPlane(t, []string{
		"restore", "db1", "latest", "--target", "/tmp/restored", "-o", "json",
		"--to-latest", "--skip-gap-check", "--force", "--force-foreign", "--verify", "require",
	}, onJob)
	if exit != 0 {
		t.Fatalf("exit=%d stderr=%s", exit, stderr)
	}
	want := map[string]any{
		"to_latest":             true,
		"skip_gap_check":        true,
		"allow_overwrite":       true,
		"allow_foreign_cluster": true,
		"verify_after":          "require",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("Args[%q] = %v, want %v (args=%v)", k, got[k], v, got)
		}
	}
}

// TestRestore_ControlPlane_InterruptCancelsRemoteJob pins M9: Ctrl-C
// while polling must exit with the aborted code (5) -- not the generic
// dispatch.poll_failed (1) -- and must ask the control plane to cancel
// the job instead of leaving the remote restore running unattended.
func TestRestore_ControlPlane_InterruptCancelsRemoteJob(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := server.New(server.Config{Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(s.Handler())
	defer hs.Close()
	// "Press Ctrl-C" once the job is queued.
	go func() {
		for ctx.Err() == nil {
			if jobs, _ := s.Jobs().List(server.ListOptions{}); len(jobs) > 0 {
				time.Sleep(50 * time.Millisecond)
				cancel()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	root := cli.NewRoot()
	var out, errb bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetContext(ctx)
	root.SetArgs([]string{"restore", "db1", "latest", "--target", "/tmp/restored", "-o", "json",
		"--control-plane", hs.URL, "--control-plane-poll-secs", "0"})
	exit := cli.Run(root)
	if exit != int(output.ExitAborted) {
		t.Errorf("exit = %d, want %d (aborted)\nstdout=%s\nstderr=%s", exit, output.ExitAborted, out.String(), errb.String())
	}
	jobs, _ := s.Jobs().List(server.ListOptions{})
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d, want 1", len(jobs))
	}
	if jobs[0].State != server.JobCancelled {
		t.Errorf("remote job state = %q, want cancelled (the interrupted CLI must cancel it)", jobs[0].State)
	}
}
