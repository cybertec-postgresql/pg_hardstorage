// pg-hardstorage-barman — drop-in replacement for the `barman` CLI.
//
// Operators symlink this binary into PATH as `barman`; existing cron
// jobs, archive_command settings, and monitoring scripts run
// unchanged but produce native pg_hardstorage backups.
//
// All implementation lives in compat/barman; main is a tiny
// dispatcher that mounts the shim's Cobra root.
package main

import (
	"fmt"
	"os"

	"github.com/cybertec-postgresql/pg_hardstorage/compat/barman"
)

func main() {
	root := barman.NewRoot(os.Stdout, os.Stderr)
	root.SetArgs(os.Args[1:])
	if _, err := root.ExecuteC(); err != nil {
		// Print shim-level errors that didn't go through
		// dispatchNative — e.g. unknown verb refusals,
		// deployment-config lookup failures, flag-validation
		// refusals.  Errors from dispatchNative carry a
		// shimError with empty message because cli.Run
		// already printed the structured-error JSON to
		// os.Stdout; we don't double-print those.
		if msg := err.Error(); msg != "" {
			fmt.Fprintln(os.Stderr, msg)
		}
		os.Exit(barman.ExitCode(err))
	}
}
