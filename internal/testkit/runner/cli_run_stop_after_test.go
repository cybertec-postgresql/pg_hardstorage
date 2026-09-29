//go:build !windows

package runner

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/scenario"
)

// fakeLongRunner stands in for `logical stream`: it runs until
// SIGTERM, then exits 0 — an operator stop is its normal end.
func fakeLongRunner(t *testing.T) *runState {
	t.Helper()
	p := filepath.Join(t.TempDir(), "agent")
	script := "#!/bin/sh\ntrap 'echo clean_stop; exit 0' TERM\nwhile :; do sleep 0.05; done\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &runState{agentBin: p}
}

func cliStep(stopAfter, timeout string) scenario.Step {
	zero := 0
	return scenario.Step{Kind: "cli_run", Args: []string{"logical", "stream", "x"},
		ExpectExit: &zero, ExpectStdoutContains: "clean_stop",
		StopAfter: stopAfter, Timeout: timeout}
}

// The logical-replication scenarios let the inactivity watchdog end a
// drained stream; once a quiet stream solicits keepalives it never
// fires, and the step hung to its deadline. stop_after ends it the
// way an operator does.
func TestCLIRun_StopAfterSendsSIGTERM(t *testing.T) {
	start := time.Now()
	r := runCLIRun(context.Background(), cliStep("300ms", "20s"), 0, fakeLongRunner(t), io.Discard)
	if !r.Pass {
		t.Fatalf("stop_after should end the command cleanly: %s", r.Message)
	}
	if el := time.Since(start); el > 10*time.Second {
		t.Errorf("stop_after did not stop the command promptly (%s)", el)
	}
}

// Without stop_after the same command runs to the deadline, and the
// failure names the timeout instead of a bare "exit -1".
func TestCLIRun_TimeoutIsNamed(t *testing.T) {
	r := runCLIRun(context.Background(), cliStep("", "300ms"), 0, fakeLongRunner(t), io.Discard)
	if r.Pass || !strings.Contains(r.Message, "timed out after 300ms") {
		t.Fatalf("want a named timeout failure, got pass=%v %q", r.Pass, r.Message)
	}
}

func TestCLIRun_StopAfterMustBeBelowTimeout(t *testing.T) {
	r := runCLIRun(context.Background(), cliStep("5s", "5s"), 0, fakeLongRunner(t), io.Discard)
	if r.Pass || !strings.Contains(r.Message, "below timeout") {
		t.Fatalf("stop_after >= timeout must be rejected, got pass=%v %q", r.Pass, r.Message)
	}
}
