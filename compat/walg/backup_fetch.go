// backup_fetch.go — WAL-G shim verb: `wal-g backup-fetch DEST BACKUP_NAME` → native `pg_hardstorage restore`.
package walg

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
)

// newBackupFetchCmd implements
//
//	wal-g backup-fetch DESTINATION_DIR BACKUP_NAME
//
// Native dispatch:
//
//	BACKUP_NAME=LATEST  → `pg_hardstorage restore <deployment> latest --target <dir> --repo ...`
//	BACKUP_NAME=<name>  → `pg_hardstorage restore <deployment> --to-backup <name> --target <dir> --repo ...`
//
// Recovery-target time / LSN are not part of WAL-G's backup-fetch
// surface (WAL-G uses recovery.signal / standby.signal in the
// destination dir for that).  The shim restores to the named
// backup; PITR continues to work via PG's native recovery_target_*
// settings.
func newBackupFetchCmd(stderr io.Writer) *cobra.Command {
	c := &cobra.Command{
		Use:           "backup-fetch DESTINATION BACKUP_NAME",
		Short:         "Restore a backup into DESTINATION",
		Args:          cobra.RangeArgs(1, 2),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBackupFetch(stderr, args)
		},
	}
	c.Flags().StringP("config", "c", "", "(silently ignored — config comes from env vars)")
	c.Flags().Bool("reverse-delta-unpack", false, "(silently ignored — native is always content-addressed)")
	return c
}

func runBackupFetch(stderr io.Writer, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("pg-hardstorage-walg: backup-fetch: DESTINATION required")
	}
	target := args[0]
	backupName := "LATEST"
	if len(args) == 2 {
		backupName = args[1]
	}

	env := loadEnv()
	native, warnings, err := mapEnvToNativeArgs("restore", env)
	if err != nil {
		return err
	}
	emitWarnings(warnings)

	// Verb argument shape: `restore <deployment> [latest|<name>]
	// --target <dir> ...flags...`.
	out := []string{native[0], env.deploymentName()}

	if strings.EqualFold(backupName, "LATEST") {
		out = append(out, "latest")
	} else {
		out = append(out, "--to-backup", backupName)
	}
	out = append(out, "--target", target)
	out = append(out, native[1:]...)

	if rc := dispatchNative(out); rc != 0 {
		return fmt.Errorf("pg-hardstorage-walg: backup-fetch: native CLI exited %d", rc)
	}
	return nil
}
