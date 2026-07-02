// wal_push.go — WAL-G shim verb: `wal-g wal-push %p` → native `wal push` (archive_command drop-in, auto-init aware).
package walg

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// newWalPushCmd implements `wal-g wal-push WAL_FILE_PATH`.
//
// PG invokes `wal-push %p` via archive_command; the positional is
// the absolute path to the WAL segment file PG just produced.
//
// Native dispatch: `pg_hardstorage wal push <deployment> <segment-path>
// --repo ...`.
//
// In a typical pg_hardstorage deployment, WAL is streamed via a
// replication slot rather than per-segment archive_command — the
// slot path is more reliable and lower-latency.  The wal-push shim
// exists for two reasons: (1) drop-in compatibility with a
// pre-existing archive_command line in postgresql.conf, and
// (2) belt-and-suspenders archiving alongside a replication slot
// for regulated environments.  Both are first-class native paths.
func newWalPushCmd(stderr io.Writer) *cobra.Command {
	c := &cobra.Command{
		Use:           "wal-push WAL_FILE_PATH",
		Short:         "Archive a single WAL segment (archive_command shim)",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWalPush(stderr, args[0])
		},
	}
	c.Flags().StringP("config", "c", "", "(silently ignored — config comes from env vars)")
	return c
}

func runWalPush(stderr io.Writer, segmentPath string) error {
	env := loadEnv()
	native, warnings, err := mapEnvToNativeArgs("wal push", env)
	if err != nil {
		return err
	}
	emitWarnings(warnings)

	// Verb shape: `wal push <deployment> <segment-path> ...flags...`.
	out := []string{native[0], "push", env.deploymentName(), segmentPath}
	out = append(out, native[1:]...)

	// Resolve the repo URL we'd auto-init against, in case the
	// native push fails with notfound.repo.  Idempotent — the
	// same buildRepoURL ran inside mapEnvToNativeArgs above; we
	// just need the bare URL string here.
	repoURL, _, urlErr := buildRepoURL(env)
	if urlErr != nil {
		// If we can't even build the URL, the auto-init path is
		// useless.  Fall back to the simple dispatch so the
		// operator sees the real error.
		if rc := dispatchNative(out); rc != 0 {
			return fmt.Errorf("pg-hardstorage-walg: wal-push: native CLI exited %d", rc)
		}
		return nil
	}

	if rc := dispatchWithAutoInit(stderr, repoURL, out); rc != 0 {
		return fmt.Errorf("pg-hardstorage-walg: wal-push: native CLI exited %d", rc)
	}
	return nil
}
