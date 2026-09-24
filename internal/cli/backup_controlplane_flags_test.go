package cli_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/server"
)

// TestBackup_ControlPlane_ForwardsSupportedFlags pins M46's forwarding
// half: --include-wal, --incremental-from and --stall-timeout reach the
// job args the agent's BackupExecutor reads (stall_timeout, not the
// inactivity_timeout key the CLI never sent).
func TestBackup_ControlPlane_ForwardsSupportedFlags(t *testing.T) {
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
		"backup", "db1", "-o", "json",
		"--include-wal", "--incremental-from", "latest", "--stall-timeout", "5m",
	}, onJob)
	if exit != 0 {
		t.Fatalf("exit=%d stderr=%s", exit, stderr)
	}
	want := map[string]any{
		"include_wal":      true,
		"incremental_from": "latest",
		"stall_timeout":    "5m0s",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("Args[%q] = %v, want %v (args=%v)", k, got[k], v, got)
		}
	}
}

// TestBackup_ControlPlane_UnsupportedFlagsRefused pins M46's refusal
// half: a flag the agent cannot honour is a usage error with nothing
// enqueued, never a silently different backup.
func TestBackup_ControlPlane_UnsupportedFlagsRefused(t *testing.T) {
	for name, extra := range map[string][]string{
		"kek":              {"--kek", "file:///k"},
		"kms-config":       {"--kms-config", "region=x"},
		"tde":              {"--tde"},
		"tde-engine":       {"--tde-engine", "pgee"},
		"tde-key-ref":      {"--tde-key-ref", "x"},
		"allow-concurrent": {"--allow-concurrent"},
		"ignore-capacity":  {"--ignore-capacity"},
		"verbose":          {"--verbose"},
		"pg-connection":    {"--pg-connection", "postgres://x@h/db"},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			exit, out, s := runCPNoAgent(t, ctx, append([]string{"backup", "db1", "-o", "json"}, extra...))
			if ctx.Err() != nil {
				t.Fatalf("--%s was silently dropped: the backup was dispatched", name)
			}
			if exit != int(output.ExitMisuse) || !strings.Contains(out, "usage.unsupported_flag") {
				t.Errorf("exit=%d, want %d with usage.unsupported_flag\n%s", exit, output.ExitMisuse, out)
			}
			if n := len(allJobs(t, s)); n != 0 {
				t.Errorf("%d job(s) enqueued; want 0", n)
			}
		})
	}
}
