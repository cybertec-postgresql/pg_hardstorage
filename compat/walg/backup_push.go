// backup_push.go — WAL-G shim verb: `wal-g backup-push DATA_DIR [--full] [--permanent]` → native backup (delta default).
package walg

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
)

// newBackupPushCmd implements `wal-g backup-push <DATA_DIR> [--full] [--permanent]`.
//
// Native dispatch:
//
//	default     → `pg_hardstorage backup <deployment> --pg-connection ... --repo ...
//	              --incremental-from latest`  (matches WAL-G's "delta when possible"
//	              default; native promotes to a full when no prior exists)
//	--full      → forces a full backup
//	--permanent → refused (legal-hold equivalent is a separate native verb)
//
// The DATA_DIR positional is consumed but not forwarded; the native
// CLI talks to PG over libpq via --pg-connection, not by reading
// the data dir on disk.  We log the value for the audit trail.
func newBackupPushCmd(stderr io.Writer) *cobra.Command {
	var full, permanent bool
	c := &cobra.Command{
		Use:           "backup-push DATA_DIR",
		Short:         "Take a backup (full or delta)",
		Args:          cobra.MaximumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBackupPush(stderr, full, permanent, args)
		},
	}
	c.Flags().BoolVar(&full, "full", false, "force a full backup")
	c.Flags().BoolVar(&permanent, "permanent", false, "(refused) use `pg_hardstorage hold add` after the backup")
	c.Flags().StringP("config", "c", "", "(silently ignored — config comes from env vars)")
	return c
}

func runBackupPush(stderr io.Writer, full, permanent bool, args []string) error {
	if permanent {
		return refuseUnimplemented(stderr, "backup-push --permanent",
			"take the backup first, then `pg_hardstorage hold add <deployment> <backup-id>`")
	}

	env := loadEnv()
	native, warnings, err := mapEnvToNativeArgs("backup", env)
	if err != nil {
		return err
	}
	emitWarnings(warnings)

	out := []string{native[0], env.deploymentName()}
	out = append(out, native[1:]...)

	if !full {
		// Match WAL-G's default: incremental relative to the
		// nearest full when one exists.  Native CLI handles the
		// "no prior full" case automatically by promoting to a
		// full.
		out = append(out, "--incremental-from", "latest")
	}

	if len(args) == 1 && args[0] != "" {
		fmt.Fprintf(stderr,
			"pg-hardstorage-walg: backup-push: DATA_DIR=%s (informational; native talks to PG over libpq)\n",
			strings.TrimRight(args[0], "/"))
	}

	// Auto-init the repo if missing — see compat/walg/autoinit.go
	// for the rationale (real wal-g auto-creates bucket
	// structure on first archive; our native CLI requires
	// `repo init` first).  Same one-shot retry the wal-push
	// shim uses.
	repoURL, _, urlErr := buildRepoURL(env)
	if urlErr != nil {
		if rc := dispatchNative(out); rc != 0 {
			return fmt.Errorf("pg-hardstorage-walg: backup-push: native CLI exited %d", rc)
		}
		return nil
	}
	if rc := dispatchWithAutoInit(stderr, repoURL, out); rc != 0 {
		return fmt.Errorf("pg-hardstorage-walg: backup-push: native CLI exited %d", rc)
	}
	return nil
}
