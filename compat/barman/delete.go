// delete.go — Barman shim verb: `barman delete <server> <backup_id>` → native `backup delete` (tombstone).
package barman

import (
	"io"

	"github.com/spf13/cobra"
)

// newDeleteCmd handles `barman delete <server> <backup_id>`.
//
// Native dispatch: `pg_hardstorage backup delete <server> <backup_id>`.
//
// The native subcommand soft-deletes (tombstones) by default —
// chunks reclaim only on the next `repo gc --apply`.  This matches
// Barman's two-phase model where `delete` removes the catalogue
// entry and a separate retention sweep reclaims data.  Operators
// scripting bulk deletes get the safer behaviour automatically.
func newDeleteCmd(stdout, stderr io.Writer) *cobra.Command {
	c := &cobra.Command{
		Use:   "delete <server> <backup_id>",
		Short: "Delete a backup (Barman compat)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			server, backupID := args[0], args[1]
			native, err := injectDeploymentFlags(
				[]string{"backup", "delete", server, backupID},
				server, false,
			)
			if err != nil {
				return err
			}
			return dispatchNative(stdout, stderr, native)
		},
	}
	c.SilenceUsage = true
	return c
}
