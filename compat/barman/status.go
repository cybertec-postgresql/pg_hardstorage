// status.go — Barman shim verb: `barman status [<server>]` → native `pg_hardstorage status`.
package barman

import (
	"io"

	"github.com/spf13/cobra"
)

// newStatusCmd handles `barman status [<server>]`.
//
// `status` is a first-class verb in real Barman 3.20 and a staple of
// nagios-style health scripts, but the shim had no entry for it at
// all: it fell through to cobra's generic `unknown command "status"`
// with no remediation — the one thing the refusal table exists to
// prevent.
//
// Native dispatch is `pg_hardstorage status [<deployment>]`, which
// reports last-backup state per deployment. Barman's own `status`
// prints a per-server summary (last backup, WAL position, retention),
// so the shapes line up; as everywhere in this shim, the equivalence
// is semantic rather than byte-for-byte (see compat/README.md).
//
// The server positional is optional here for the same reason it is in
// real barman: `barman status` with no server summarises every
// configured server.
func newStatusCmd(stdout, stderr io.Writer) *cobra.Command {
	c := &cobra.Command{
		Use:   "status [<server>]",
		Short: "Show last-backup state for a server (Barman compat)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			native := []string{"status"}
			if len(args) == 1 {
				// Native `status [<deployment>]` takes the deployment
				// as a positional and reads the repo from config, so
				// no flag injection is needed (and --repo would be
				// rejected as unknown).
				native = append(native, args[0])
			}
			return dispatchNative(stdout, stderr, native)
		},
	}
	c.SilenceUsage = true
	return c
}
