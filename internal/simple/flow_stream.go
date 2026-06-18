// flow_stream.go — simple-CLI flow #3: starts continuous protection by exec'ing `wal stream`.
package simple

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
)

// flowStream is operation #3 — "Start continuous protection".
//
// The wal-stream process is long-running and structured around a
// process-level cancellation contract (SIGTERM = graceful flush);
// rather than re-import its runner from this short-lived binary,
// we exec the full pg_hardstorage binary and inherit its stdout
// so the operator sees the same progress lines they would in a
// regular shell.
//
// This is the only flow that subprocesses out — every other one is
// a Go function call into internal/.  The trade-off is deliberate:
// wal-stream's lifecycle is "run forever, gracefully stop on
// Ctrl-C", and replicating that in-process means owning a goroutine
// for the duration of the menu loop, which the design doc
// (specifically the "no background state" point) wants us to avoid.
type flowStream struct{}

// Name implements Flow; returns the menu label printed before this
// operation runs.
func (flowStream) Name() string { return "start WAL streaming" }

// Run implements Flow; locates the full pg_hardstorage binary and
// execs `pg_hardstorage wal stream <deployment>` with stdout/stderr
// inherited so the operator sees streaming progress directly.
// Ctrl-C (exit code 130) is treated as a clean stop.
func (f flowStream) Run(ctx context.Context, env *Env) error {
	dep, err := pickDeployment(env, "Which database should we stream WAL for?")
	if err != nil {
		if errors.Is(err, errNoDeployments) {
			env.Prompter.Println("  no deployments yet — pick #1 from the menu to set one up.")
			return nil
		}
		return err
	}

	// Find the pg_hardstorage sibling binary.  The simple binary
	// ships next to it in `bin/`; falling back to PATH means
	// distros that installed both system-wide just work.
	agent, err := findAgentBinary()
	if err != nil {
		return err
	}

	env.Prompter.Printf("\n  About to start WAL streaming for %s.\n", dep.Name)
	env.Prompter.Printf("    This will run in this terminal until you press Ctrl-C.\n")
	env.Prompter.Printf("    Streamed WAL segments land in the repo so subsequent\n")
	env.Prompter.Printf("    restores can recover past the last basebackup.\n\n")
	ok, err := env.Prompter.YesNo("Continue?", true)
	if err != nil {
		return err
	}
	if !ok {
		env.Prompter.Println("  cancelled.")
		return nil
	}

	cmd := exec.CommandContext(ctx, agent,
		"wal", "stream", dep.Name,
		"--pg-connection", dep.PGConnection,
		"--repo", dep.Repo,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	env.Prompter.Println("  $ pg_hardstorage wal stream ...")
	if err := cmd.Run(); err != nil {
		// Ctrl-C surfaces as exit code 130; treat that as the
		// "operator stopped me" path and report cleanly.
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 130 {
			env.Prompter.Println("\n  stopped.")
			env.State.LastDeployment = dep.Name
			return nil
		}
		return err
	}
	env.Prompter.Println("\n  wal stream exited cleanly.")
	env.State.LastDeployment = dep.Name
	return nil
}

// findAgentBinary locates the full pg_hardstorage binary.  Order:
//
//  1. PG_HARDSTORAGE_BIN env var (lets the testkit override)
//  2. The same directory the simple binary lives in
//  3. PATH lookup
//
// Returns a clear error when nothing matches so the operator knows
// to add it to PATH or set the env var.
func findAgentBinary() (string, error) {
	if b := os.Getenv("PG_HARDSTORAGE_BIN"); b != "" {
		return b, nil
	}
	self, err := os.Executable()
	if err == nil {
		candidate := selfDirJoin(self, "pg_hardstorage")
		if _, statErr := os.Stat(candidate); statErr == nil {
			return candidate, nil
		}
	}
	if b, err := exec.LookPath("pg_hardstorage"); err == nil {
		return b, nil
	}
	return "", fmt.Errorf("pg_hardstorage binary not found; install it or set PG_HARDSTORAGE_BIN")
}

// selfDirJoin returns <dirname of self>/<name>.  Tiny helper that
// avoids importing path/filepath just for one Join + Dir pair (the
// rest of this file doesn't need it).
func selfDirJoin(self, name string) string {
	for i := len(self) - 1; i >= 0; i-- {
		if self[i] == '/' {
			return self[:i+1] + name
		}
	}
	return name
}
