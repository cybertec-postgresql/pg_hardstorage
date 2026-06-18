// Package tools' CLIRunner is the self-invocation primitive every
// "read live state" tool routes through.  Rather than refactor each
// CLI subcommand into a reusable Go primitive (which would double
// our maintenance surface), the LLM tools shell out to the same
// binary that hosts the LLM session and parse its v1-stable JSON
// output.
//
// Why this is the right shape:
//
//   - Every CLI command already emits a typed Result body keyed by
//     pg_hardstorage.v1; the schema is the same one external
//     scripts / monitoring tools consume.  Tools that read that
//     output benefit from the same 24-month back-compat commitment.
//   - Tests stub via a mock runner without forking the binary.
//   - When the operator upgrades the binary, the LLM tools track
//     the upgraded JSON shape automatically — no separate package
//     to keep in sync.
//   - The CLI's own preflight, RBAC, and error-mapping all run
//     through unchanged.  An LLM that can't read a backup without
//     the operator's tenant scope can't pretend it can.
//
// What this is NOT: a way to mutate state.  CLIRunner explicitly
// asserts that every invocation routes through a read-only
// command path; mutation tools are gated behind a separate
// runtime mode that lands.

package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// CLIRunner runs subcommands against the same pg_hardstorage binary
// that hosts the LLM session.  Path is the absolute path resolved
// at startup via os.Executable so tools can't be tricked into
// invoking a different binary by a $PATH rewrite mid-session.
type CLIRunner struct {
	// Path is the absolute path to the pg_hardstorage binary.  Use
	// ResolveSelf to populate this; tests substitute a stub.
	Path string

	// Env is the extra env injected into every invocation.  Empty
	// inherits the parent process env (the typical case).  Tests
	// use this to point invocations at a sandboxed home dir.
	Env []string

	// Timeout caps any single subcommand invocation.  Defaults to
	// DefaultRunTimeout when zero.  Cancels the underlying process
	// if exceeded.
	Timeout time.Duration

	// Runner is the override hook tests use to substitute a stub
	// without fork+exec.  Production callers leave this nil; the
	// default invokes os/exec.Command.
	Runner func(ctx context.Context, args []string) ([]byte, []byte, int, error)
}

// DefaultRunTimeout is the per-invocation timeout when CLIRunner.Timeout is zero.
const DefaultRunTimeout = 30 * time.Second

// ResolveSelf returns the absolute path to the running binary.
// Wrapper around os.Executable for testability + clearer error
// messages (the bare error is "executable file not found" with no
// context about why the LLM cares).
func ResolveSelf() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("llm tools: resolve self path: %w (the LLM helper needs to know which pg_hardstorage binary it is hosted by; cannot proceed without it)", err)
	}
	return p, nil
}

// ErrNonZeroExit is returned when the subcommand exits with a
// non-zero status.  The error wraps the exit code; callers can
// unwrap to inspect.  Stdout + stderr are surfaced in the message
// so a partial JSON body or structured error code is visible to
// the model.
var ErrNonZeroExit = errors.New("llm tools: subcommand exited non-zero")

// RunJSON invokes the binary with the given args + appends `-o
// json` and returns the stdout body.  Refuses to run when the
// args contain a known mutation verb (`-apply`, `--yes`,
// `--force`, etc.); the gate is local-only — the binary's own
// safety machinery is the authoritative defence — but it shaves
// off easy operator mistakes.
//
// Returns (stdout, error).  stderr is folded into the error on a
// non-zero exit; the caller can read it via errors.As against an
// *ExitError.
func (r *CLIRunner) RunJSON(ctx context.Context, args ...string) ([]byte, error) {
	if r == nil {
		return nil, errors.New("llm tools: CLIRunner is nil")
	}
	if r.Path == "" {
		return nil, errors.New("llm tools: CLIRunner.Path is empty (call ResolveSelf at startup)")
	}
	if err := assertReadOnlyArgs(args); err != nil {
		return nil, err
	}
	timeout := r.Timeout
	if timeout == 0 {
		timeout = DefaultRunTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	full := append([]string{}, args...)
	full = appendOutputJSON(full)

	if r.Runner != nil {
		stdout, stderr, code, err := r.Runner(cctx, full)
		if err != nil {
			return stdout, fmt.Errorf("llm tools: runner stub: %w", err)
		}
		if code != 0 {
			return stdout, fmt.Errorf("%w: exit %d: %s", ErrNonZeroExit, code, strings.TrimSpace(string(stderr)))
		}
		return stdout, nil
	}

	cmd := exec.CommandContext(cctx, r.Path, full...)
	if len(r.Env) > 0 {
		cmd.Env = append(os.Environ(), r.Env...)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return stdout.Bytes(), fmt.Errorf("%w: exit %d: %s", ErrNonZeroExit, ee.ExitCode(), strings.TrimSpace(stderr.String()))
		}
		return stdout.Bytes(), fmt.Errorf("llm tools: exec %s: %w", strings.Join(full, " "), err)
	}
	return stdout.Bytes(), nil
}

// appendOutputJSON appends `-o json` if the caller hasn't already
// requested an output mode.  Idempotent.
func appendOutputJSON(args []string) []string {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-o", "--output":
			return args
		}
		if strings.HasPrefix(args[i], "--output=") || strings.HasPrefix(args[i], "-o=") {
			return args
		}
	}
	return append(args, "-o", "json")
}

// assertReadOnlyArgs is the local belt-and-braces refusal of
// mutation flags.  Authoritative defence is the binary's own
// approval gates / typed-confirmation flags / RBAC; this layer
// just shaves off the easy mistakes in case a future tool
// accidentally constructs a mutation command.
//
// The list mirrors the operator-facing "this MUTATES state" flags
// the binary refuses to run without explicit acknowledgement.
// Anything not on this list is allowed through (read-only by
// default).
func assertReadOnlyArgs(args []string) error {
	for _, a := range args {
		switch a {
		case "--apply", "--yes", "--force", "--reset-chain-staging",
			"--confirm-keyring", "--require-approval":
			return fmt.Errorf("llm tools: refuse to invoke mutation flag %q via the read-only tool surface (skills are read-only; advise+execute lands with its own dispatch path)", a)
		}
	}
	return nil
}
