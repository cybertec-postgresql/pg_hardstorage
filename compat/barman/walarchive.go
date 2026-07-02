// walarchive.go — Barman shim companion: `barman-wal-archive <server> <segment>` → native `wal push` for archive_command.
package barman

import (
	"io"

	"github.com/spf13/cobra"
)

// NewWALArchiveRoot returns the Cobra command tree for the
// `barman-wal-archive` companion binary, which Barman ships as a
// separate executable that PG's archive_command invokes.  We mirror
// the executable boundary so symlink installs work bit-for-bit:
//
//	ln -s /usr/lib/pg_hardstorage/bin/pg-hardstorage-barman-wal-archive \
//	      /usr/bin/barman-wal-archive
//
// Native dispatch: `pg_hardstorage wal push <server> <segment-path>`.
//
// archive_command examples that already exist on operator machines
// keep working unchanged:
//
//	archive_command = 'barman-wal-archive db1 %p'
func NewWALArchiveRoot(stdout, stderr io.Writer) *cobra.Command {
	c := &cobra.Command{
		Use:   "barman-wal-archive <barman-host> <server-name> <wal-path>",
		Short: "Archive one PostgreSQL WAL segment (Barman compat)",
		Long: `barman-wal-archive is invoked by PostgreSQL's archive_command
during normal operation.  In the pg_hardstorage shim, it dispatches
the segment into the native repository via 'pg_hardstorage wal push'.

Real barman-wal-archive takes three positionals — BARMAN_HOST,
SERVER_NAME, and WAL_PATH:

	archive_command = 'barman-wal-archive backup.internal db1 %p'

BARMAN_HOST is the SSH target the upstream tool ships the segment to;
the shim archives straight into the configured repository over libpq,
so BARMAN_HOST is accepted for argv compatibility and ignored. The
SERVER_NAME positional is the deployment and WAL_PATH is the segment.

Re-archives of an already-committed segment are no-ops; PG's retry
loop is safe.`,
		Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			// args[0] = BARMAN_HOST (SSH target — ignored by the shim),
			// args[1] = SERVER_NAME (deployment), args[2] = WAL_PATH.
			server, segPath := args[1], args[2]
			// wal push derives system_identifier from the segment
			// header (issue #8) so --pg-connection is unnecessary.
			// Skip it from config so a deployment without a
			// configured DSN still archives — the only case
			// pg-connection helps is a corrupt segment, which the
			// operator handles by re-running with the flag
			// explicitly.
			native, err := injectDeploymentFlags(
				[]string{"wal", "push", server, segPath},
				server, false,
			)
			if err != nil {
				return err
			}
			return dispatchNative(stdout, stderr, native)
		},
	}
	c.SilenceUsage = true
	c.SilenceErrors = true
	return c
}
