// dispatch.go — Barman shim dispatcher: indirects verb argv into internal/cli.Run, swappable for tests.
package barman

import (
	"io"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/cli"
)

// dispatcherFunc is the function signature dispatchNative resolves
// to.  Tests swap this for a capturing implementation that records
// the synthetic argv without spinning up the real CLI runner.
type dispatcherFunc func(stdout, stderr io.Writer, args []string) error

// dispatcher is the active implementation.  Production points it at
// the real CLI; tests overwrite it via swapDispatcher.
var dispatcher dispatcherFunc = realDispatch

// dispatchNative is the verb files' single entry point.  Indirecting
// through `dispatcher` keeps the verb code untouched and lets tests
// assert the rendered argv.
func dispatchNative(stdout, stderr io.Writer, args []string) error {
	return dispatcher(stdout, stderr, args)
}

// realDispatch runs the native pg_hardstorage CLI with the given
// synthetic argv.  All output flows through the Cobra command's
// own writers, which we point at the shim's stdout / stderr so the
// caller (cron, monitoring) sees the native renderer's output
// transparently.
//
// We construct a fresh root each call to avoid accidental flag
// state leaking across two shim verbs in the same process — Cobra's
// flags are stateful and a single root is not safe for re-use.
//
// Goes through cli.Run rather than root.ExecuteC: the native
// dispatcher's structured-error renderer fires inside Run, NOT
// inside ExecuteC.  Both the shim root and the native root carry
// SilenceErrors=true, so calling ExecuteC directly bubbles errors
// up but never prints them — operators saw `barman <verb>` exit
// non-zero with empty stdout/stderr (verified with strace: zero
// writes on the failure path).  Mirrors the pgbackrest shim.
func realDispatch(stdout, stderr io.Writer, args []string) error {
	root := cli.NewRoot()
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetArgs(args)
	if rc := cli.Run(root); rc != 0 {
		return &shimError{exitCode: rc, message: ""}
	}
	return nil
}

// swapDispatcher temporarily replaces the dispatcher; the returned
// closure restores the previous value (deferable from tests).
func swapDispatcher(d dispatcherFunc) func() {
	prev := dispatcher
	dispatcher = d
	return func() { dispatcher = prev }
}
