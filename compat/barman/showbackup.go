// showbackup.go — Barman shim verb: `barman show-backup <server> <backup_id>` → native `pg_hardstorage show`.
package barman

import (
	"io"

	"github.com/spf13/cobra"
)

// newShowBackupCmd handles `barman show-backup <server> <backup_id>`.
//
// Native dispatch: `pg_hardstorage show <server> <backup_id>`.
//
// Barman's "latest" alias maps to the native "latest" alias —
// both tools accept it for the same purpose.
func newShowBackupCmd(stdout, stderr io.Writer) *cobra.Command {
	c := &cobra.Command{
		Use:   "show-backup <server> <backup_id>",
		Short: "Show details of a single backup (Barman compat)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			server, backupID := args[0], args[1]
			native, err := injectDeploymentFlags([]string{"show", server, backupID}, server, false)
			if err != nil {
				return err
			}
			return dispatchNative(stdout, stderr, native)
		},
	}
	c.SilenceUsage = true
	return c
}
