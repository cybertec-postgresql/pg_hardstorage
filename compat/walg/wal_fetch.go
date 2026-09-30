// wal_fetch.go — WAL-G shim verb: `wal-g wal-fetch %f %p` → native `wal fetch` (restore_command drop-in).
package walg

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
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

// exitAbortRecovery is the status that makes PostgreSQL treat a failed
// restore_command as fatal rather than as the end of the archive: its
// RestoreArchivedFile classifies the result with
// wait_result_is_any_signal(rc, true), which is true for any exit
// status greater than 125. Same value and rationale as
// compat/barmancloud's wal-restore and the shell wrapper in
// internal/restore/walfetchcmd/tail.go.
const exitAbortRecovery = 126

// exitSegmentAbsent is PostgreSQL's ordinary "segment not in the
// archive" answer — at the end of an unbounded recovery it means
// "stop replaying and promote".
const exitSegmentAbsent = 1

// restoreCommandExit is the three-way exit-code contract every
// restore_command shim speaks:
//
//	native 0            → exit 0   (segment delivered)
//	native 6 (notfound) → exit 1   (the genuine "no such segment")
//	anything else       → exit 126 (recovery ABORTS loudly)
//
// Collapsing every failure to 1 made an S3 503, an expired credential
// or a missing WALG_* variable read to PostgreSQL as a clean end of
// archive: the server promoted with unreplayed WAL still in the
// repository.
func restoreCommandExit(nativeRC int) int {
	switch nativeRC {
	case 0:
		return 0
	case int(output.ExitNotFound):
		return exitSegmentAbsent
	default:
		return exitAbortRecovery
	}
}

// abortRecovery wraps a wal-fetch failure that never reached the
// repository (bad argv, missing env) so it exits 126, not 1.
func abortRecovery(stderr io.Writer, err error) error {
	msg := fmt.Sprintf("%v\npg-hardstorage-walg: wal-fetch: exiting %d to ABORT recovery — this is a fetch FAILURE, "+
		"not an end of archive; PostgreSQL must not promote here", err, exitAbortRecovery)
	fmt.Fprintln(stderr, msg)
	return &shimError{exitCode: exitAbortRecovery, message: msg}
}

func runWalFetch(stderr io.Writer, segName, outputPath string) error {
	env := loadEnv()
	native, warnings, err := mapEnvToNativeArgs("wal fetch", env)
	if err != nil {
		return abortRecovery(stderr, err)
	}
	emitWarnings(warnings)

	// Verb shape: `wal fetch <deployment> <segment-name>
	// <output-path> ...flags...`.
	out := []string{native[0], "fetch", env.deploymentName(), segName, outputPath}
	out = append(out, native[1:]...)

	rc := dispatchNative(out)
	switch code := restoreCommandExit(rc); code {
	case 0:
		return nil
	case exitSegmentAbsent:
		msg := fmt.Sprintf("pg-hardstorage-walg: wal-fetch: %s not in the archive (native CLI exited %d)", segName, rc)
		fmt.Fprintln(stderr, msg)
		return &shimError{exitCode: code, message: msg}
	default:
		return abortRecovery(stderr, fmt.Errorf("pg-hardstorage-walg: wal-fetch: native CLI exited %d", rc))
	}
}
