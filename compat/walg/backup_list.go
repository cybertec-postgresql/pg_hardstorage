// backup_list.go — WAL-G shim verb: `wal-g backup-list [--json]` → native `pg_hardstorage list`.
package walg

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// newBackupListCmd implements `wal-g backup-list`.
//
// Native dispatch: `pg_hardstorage list <deployment> --repo ...`.
// WAL-G accepts --pretty, --json, --detail flags; we honour --json
// (maps to native `-o json`) and ignore the others.
func newBackupListCmd(stderr io.Writer) *cobra.Command {
	var jsonOut bool
	c := &cobra.Command{
		Use:           "backup-list",
		Short:         "List backups in the repository",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runBackupList(stderr, jsonOut)
		},
	}
	c.Flags().BoolVar(&jsonOut, "json", false, "emit JSON (maps to native -o json)")
	c.Flags().Bool("pretty", false, "(silently ignored — native pretty-prints by default)")
	c.Flags().Bool("detail", false, "(silently ignored — native list shows full detail)")
	c.Flags().StringP("config", "c", "", "(silently ignored — config comes from env vars)")
	return c
}

func runBackupList(stderr io.Writer, jsonOut bool) error {
	env := loadEnv()
	native, warnings, err := mapEnvToNativeArgs("list", env)
	if err != nil {
		return err
	}
	emitWarnings(warnings)

	out := []string{native[0], env.deploymentName()}
	out = append(out, native[1:]...)
	if jsonOut {
		out = append(out, "-o", "json")
	}

	if rc := dispatchNative(out); rc != 0 {
		return fmt.Errorf("pg-hardstorage-walg: backup-list: native CLI exited %d", rc)
	}
	return nil
}
