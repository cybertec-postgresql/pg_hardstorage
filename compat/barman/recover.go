// recover.go — Barman shim verb: `barman recover` → native `pg_hardstorage restore` with PITR flag translation.
package barman

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// newRecoverCmd handles `barman recover <server> <backup_id> <target_dir>`.
//
// Native dispatch: `pg_hardstorage restore <server> <backup_id> --target <target_dir> [translated PITR flags]`.
//
// Flag translation runs through compat/barman/flags.go's table.
func newRecoverCmd(stdout, stderr io.Writer) *cobra.Command {
	var (
		targetTime   string
		targetXID    string
		targetName   string
		targetImmed  bool
		targetAction string
		remoteSSHCmd string
		getWAL       bool
		noGetWAL     bool
		retryTimes   int
		retrySleep   int
	)
	c := &cobra.Command{
		Use:   "recover <server> <backup_id> <target_dir>",
		Short: "Recover a backup to a target directory (Barman compat)",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			server, backupID, targetDir := args[0], args[1], args[2]

			r := &recoverArgs{
				server:       server,
				backupID:     backupID,
				targetDir:    targetDir,
				targetTime:   targetTime,
				targetXID:    targetXID,
				targetName:   targetName,
				targetImmed:  targetImmed,
				targetAction: targetAction,
				remoteSSHCmd: remoteSSHCmd,
			}
			switch {
			case getWAL:
				v := true
				r.getWAL = &v
			case noGetWAL:
				v := false
				r.getWAL = &v
			}
			if retryTimes > 0 {
				r.retryN = "set"
			}
			if retrySleep > 0 {
				r.retrySlp = "set"
			}

			translated, err := mapToNativeArgs(r, stderr)
			if err != nil {
				// Print to stderr ourselves — root has
				// SilenceErrors=true so cobra wouldn't.
				fmt.Fprintf(stderr, "pg-hardstorage-barman: recover: %v\n", err)
				return err
			}

			native := []string{"restore", server, backupID, "--target", targetDir}
			native = append(native, translated...)
			// Native `restore` accepts --repo but NOT --pg-connection
			// (it talks to the repository, not a live PG). Inject --repo
			// only; passing --pg-connection here makes cobra reject the
			// argv as an unknown flag.
			native, err = injectDeploymentFlags(native, server, false)
			if err != nil {
				return err
			}
			return dispatchNative(stdout, stderr, native)
		},
	}
	c.Flags().StringVar(&targetTime, "target-time", "", "Barman: PITR — recover to this timestamp")
	c.Flags().StringVar(&targetXID, "target-xid", "", "Barman: PITR — recover to this XID (refused; use --target-time / --to-lsn)")
	c.Flags().StringVar(&targetName, "target-name", "", "Barman: PITR — recover to a named restore point")
	c.Flags().BoolVar(&targetImmed, "target-immediate", false, "Barman: stop as soon as consistent")
	c.Flags().StringVar(&targetAction, "target-action", "", "Barman: target action (pause|promote|shutdown)")
	c.Flags().StringVar(&remoteSSHCmd, "remote-ssh-command", "", "Barman: SSH command for remote recover (ignored)")
	c.Flags().BoolVar(&getWAL, "get-wal", false, "Barman: fetch WAL during recovery (ignored — always on)")
	c.Flags().BoolVar(&noGetWAL, "no-get-wal", false, "Barman: skip WAL fetch (ignored)")
	c.Flags().IntVar(&retryTimes, "retry-times", 0, "Barman: retry count (ignored)")
	c.Flags().IntVar(&retrySleep, "retry-sleep", 0, "Barman: retry sleep secs (ignored)")
	c.SilenceUsage = true
	return c
}
