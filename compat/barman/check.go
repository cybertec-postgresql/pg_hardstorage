// check.go — Barman shim verb: `barman check <server>` → native `pg_hardstorage doctor` (incl. --nagios).
package barman

import (
	"io"

	"github.com/spf13/cobra"
)

// newCheckCmd handles `barman check <server>`.
//
// Native dispatch: `pg_hardstorage doctor <server>`.
//
// Barman's `check` and the native `doctor` overlap heavily —
// both audit connectivity, replication slot health, repository
// reachability, and config sanity.  The native doctor reports a few
// extra items (KEK presence, manifest signature roots) that Barman
// users will see as "more checks, same shape".
//
// `--nagios` is honoured by switching the native output to a
// Nagios-friendly template line; existing `barman check --nagios`
// monitoring lines keep working without script changes.
func newCheckCmd(stdout, stderr io.Writer) *cobra.Command {
	var nagios bool
	c := &cobra.Command{
		Use:   "check <server>",
		Short: "Health-check a server (Barman compat)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			server := args[0]
			// Native `doctor [<deployment>]` takes the deployment as a
			// positional and registers NO --repo / --pg-connection flags
			// (only --exit-on-issues plus the persistent --output/--template).
			// Do NOT inject deployment flags here — cobra rejects them as
			// unknown. The server name is already the doctor positional.
			native := []string{"doctor", server}
			if nagios {
				// Nagios convention: a single line "<SERVICE> OK|WARNING|CRITICAL ..."
				native = append(native,
					"--output", "template",
					"--template", `{{if .ok}}BARMAN OK - {{.summary}}{{else}}BARMAN CRITICAL - {{.summary}}{{end}}`,
				)
			}
			return dispatchNative(stdout, stderr, native)
		},
	}
	c.Flags().BoolVar(&nagios, "nagios", false, "Barman: emit a single-line Nagios-format result")
	c.SilenceUsage = true
	return c
}
