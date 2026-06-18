// watch.go — `watch` subcommand: live TUI that tails events.ndjson from an in-progress soak or scenario run.
package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/testkit/watch"
)

// newWatchCmd renders a live TUI of an in-progress (or
// finished) soak / scenario run.
//
// Side-process by design: the soak emits events.ndjson to its
// report dir; `testkit watch <run-dir>` opens that file in
// another terminal and tails it.  This keeps the soak's
// stdout NDJSON stream untouched (CI pipelines piping through
// jq keep working) and lets operators attach / detach the live
// view without disturbing the run.
func newWatchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "watch <run-dir | events-file>",
		Short: "Live TUI for an in-progress soak or scenario run",
		Long: `Tails the events.ndjson (soak) or result.ndjson (scenario)
emitted by the testkit and renders a per-cell dashboard.

The argument can be:
  - a run directory (looks for events.ndjson, then result.ndjson)
  - a path directly to either file

The watcher is a separate process from the soak/scenario it's
observing — start the soak, then in another terminal:

    pg_hardstorage_testkit watch ./test-runs/run-20260506-XXXXXX

Quit with q, esc, or ctrl-c.  The soak keeps running.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := watch.ResolveEventsPath(args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "watching %s\n", path)
			return watch.Run(cmd.Context(), path)
		},
	}
}
