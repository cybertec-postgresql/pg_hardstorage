// backup.go — Barman shim verb: `barman backup <server>` → native `pg_hardstorage backup`.
package barman

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// newBackupCmd handles `barman backup <server>`.
//
// Native dispatch: `pg_hardstorage backup <server>`.
//
// We strip out Barman-specific knobs that don't translate (job
// concurrency, jobs file, immediate-checkpoint flag) — most of them
// either have no equivalent or are picked up automatically by the
// native runner from the deployment config.
func newBackupCmd(stdout, stderr io.Writer) *cobra.Command {
	var (
		jobs          int
		immediateCKPT bool
		waitForWAL    bool
		retryTimes    int
		retrySleep    int
		reuseBackup   string
	)
	c := &cobra.Command{
		Use:   "backup <server>",
		Short: "Take a backup of the named server (Barman compat)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			server := args[0]
			// Barman's `--reuse-backup={link,copy}` requests its
			// own brand of incremental: hard-link (or copy)
			// unchanged files from the previous backup into the
			// new one.  pg_hardstorage does NOT implement that
			// scheme — its incremental support routes through PG
			// 17's BASE_BACKUP INCREMENTAL + pg_combinebackup,
			// which is a different storage shape entirely (the
			// repo stores delta files, not link farms).  Silently
			// accepting --reuse-backup and falling back to a full
			// would betray the operator's expectation; refusing
			// loudly with remediation is the honest stance.
			//
			// --reuse-backup=off is the explicit "no reuse"
			// choice and matches our default, so we accept it as
			// a no-op for migration ergonomics.
			switch reuseBackup {
			case "", "off":
				// no-op
			case "link", "copy":
				return fmt.Errorf(
					"--reuse-backup=%s not supported by pg_hardstorage; "+
						"Barman's link/copy incremental scheme has no "+
						"pg_hardstorage equivalent.  For PG 17+ incremental "+
						"backups, use the native CLI: "+
						"`pg_hardstorage backup %s --incremental-from <prior-backup-id>` "+
						"(see docs/how-to/migration/from-barman.md for the migration recipe)",
					reuseBackup, server)
			default:
				return fmt.Errorf(
					"--reuse-backup=%q: unknown value (Barman accepts off|link|copy; "+
						"only `off` translates to pg_hardstorage)",
					reuseBackup)
			}
			if jobs > 0 {
				warnDroppedFlag(stderr, fmt.Sprintf("--jobs=%d", jobs),
					"native runner picks parallelism from deployment.parallelism")
			}
			if waitForWAL {
				warnDroppedFlag(stderr, "--wait",
					"native always waits for the segment containing the stop LSN before committing")
			}
			if retryTimes > 0 || retrySleep > 0 {
				warnDroppedFlag(stderr, "--retry-times/--retry-sleep",
					"native has built-in exponential backoff")
			}
			native := []string{"backup", server}
			if immediateCKPT {
				native = append(native, "--fast")
			}
			native, err := injectDeploymentFlags(native, server, true)
			if err != nil {
				return err
			}
			return dispatchNative(stdout, stderr, native)
		},
	}
	c.Flags().IntVar(&jobs, "jobs", 0, "Barman: parallel jobs (ignored — set deployment.parallelism)")
	c.Flags().BoolVar(&immediateCKPT, "immediate-checkpoint", false, "Barman: force immediate CHECKPOINT")
	c.Flags().BoolVar(&waitForWAL, "wait", false, "Barman: wait for WAL after backup (always on)")
	c.Flags().IntVar(&retryTimes, "retry-times", 0, "Barman: retry count (ignored)")
	c.Flags().IntVar(&retrySleep, "retry-sleep", 0, "Barman: retry sleep secs (ignored)")
	c.Flags().StringVar(&reuseBackup, "reuse-backup", "",
		"Barman: incremental scheme (off|link|copy). Only `off` is supported; "+
			"`link` and `copy` refuse with a remediation pointing to "+
			"`pg_hardstorage backup --incremental-from`")
	c.SilenceUsage = true
	return c
}
