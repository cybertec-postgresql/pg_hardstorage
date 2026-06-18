// wal_fetch.go — WAL-G shim verb: `wal-g wal-fetch %f %p` → native `wal fetch` (restore_command drop-in).
package walg

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// newWalFetchCmd implements `wal-g wal-fetch WAL_FILE_NAME OUTPUT_PATH`.
//
// PG invokes `wal-fetch %f %p` via restore_command; the first
// positional is the segment name (e.g. 000000010000000000000003)
// and the second is the absolute path the segment must land at.
//
// Native dispatch: `pg_hardstorage wal fetch <deployment> <segment-name>
// <output-path> --repo ...`.
func newWalFetchCmd(stderr io.Writer) *cobra.Command {
	c := &cobra.Command{
		Use:           "wal-fetch WAL_FILE_NAME OUTPUT_PATH",
		Short:         "Restore a single WAL segment (restore_command shim)",
		Args:          cobra.ExactArgs(2),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWalFetch(stderr, args[0], args[1])
		},
	}
	c.Flags().StringP("config", "c", "", "(silently ignored — config comes from env vars)")
	return c
}

func runWalFetch(stderr io.Writer, segName, outputPath string) error {
	env := loadEnv()
	native, warnings, err := mapEnvToNativeArgs("wal", env)
	if err != nil {
		return err
	}
	emitWarnings(warnings)

	// Verb shape: `wal fetch <deployment> <segment-name>
	// <output-path> ...flags...`.
	out := []string{native[0], "fetch", env.deploymentName(), segName, outputPath}
	out = append(out, native[1:]...)

	if rc := dispatchNative(out); rc != 0 {
		return fmt.Errorf("pg-hardstorage-walg: wal-fetch: native CLI exited %d", rc)
	}
	return nil
}
