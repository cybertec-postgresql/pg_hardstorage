// dispatch.go — WAL-G shim dispatcher: runs synthetic argv via internal/cli.Run with optional stdout/stderr capture.
package walg

import (
	"bytes"
	"os"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/cli"
)

// dispatchNative runs the native CLI with the supplied synthetic
// argv.  Verbs build their argv via mapEnvToNativeArgs + verb-
// specific positionals, then call this.
//
// We always go through internal/cli.Run rather than calling
// internal/backup or internal/wal directly — the shim's promise is
// "tracks the public CLI surface", not "duplicates the internal Go
// API".  Tests substitute a stub.
//
// Returns the native CLI's process-style exit code (0 on success,
// non-zero on failure).
var dispatchNative = func(args []string) int {
	root := cli.NewRoot()
	root.SetArgs(args)
	return cli.Run(root)
}

// dispatchResult bundles the exit code with the captured
// stdout / stderr from one native CLI invocation.  Used by the
// auto-init wrapper so the shim can recognise specific error
// codes (notfound.repo, conflict.repo_exists) without parsing
// fragile string output.
type dispatchResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
}

// dispatchNativeCapture runs the native CLI with stdout +
// stderr captured into byte buffers.  Used by autoinit.go to
// peek at the structured-error JSON the native CLI emits on
// failure (well-known schema with a `code` field — see
// internal/output/error.go).  The captured streams are
// forwarded to os.Stdout / os.Stderr by the caller so the
// operator's view is unchanged on the success path.
//
// var-typed so tests can substitute a stub.
var dispatchNativeCapture = func(args []string) dispatchResult {
	root := cli.NewRoot()
	var outBuf, errBuf bytes.Buffer
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	root.SetArgs(args)
	rc := cli.Run(root)
	return dispatchResult{
		ExitCode: rc,
		Stdout:   outBuf.Bytes(),
		Stderr:   errBuf.Bytes(),
	}
}

// forwardCaptured replays a captured dispatch's stdout / stderr
// to the real process file descriptors.  Used after a successful
// dispatch (or one we're about to error on) so the operator
// sees the native CLI's normal output even when the shim
// captured it for inspection.
func forwardCaptured(res dispatchResult) {
	if len(res.Stdout) > 0 {
		_, _ = os.Stdout.Write(res.Stdout)
	}
	if len(res.Stderr) > 0 {
		_, _ = os.Stderr.Write(res.Stderr)
	}
}
