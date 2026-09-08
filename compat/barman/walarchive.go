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
		Use:   "barman-wal-archive [<barman-host>] <server-name> <wal-path>",
		Short: "Archive one PostgreSQL WAL segment (Barman compat)",
		Long: `barman-wal-archive is invoked by PostgreSQL's archive_command
during normal operation.  In the pg_hardstorage shim, it dispatches
the segment into the native repository via 'pg_hardstorage wal push'.

BOTH real-world argv shapes are accepted:

	archive_command = 'barman-wal-archive db1 %p'                     # 2 args
	archive_command = 'barman-wal-archive backup.internal db1 %p'     # 3 args

The 2-argument form (SERVER_NAME, WAL_PATH) is the classic shape that
sits in most existing postgresql.conf files. The 3-argument form adds
BARMAN_HOST first: that is the SSH target the upstream tool ships the
segment to, and since the shim archives straight into the configured
repository over libpq, it is accepted for argv compatibility and
ignored.

Re-archives of an already-committed segment are no-ops; PG's retry
loop is safe.`,
		// 2 or 3 positionals. Requiring exactly 3 broke the single most
		// common drop-in point there is: an existing archive_command
		// written in the 2-arg form failed immediately with
		// "accepts 3 arg(s), received 2" — and PostgreSQL retries a
		// failing archive_command forever, so WAL piled up.
		Args: cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			// 3-arg: BARMAN_HOST (SSH target — ignored by the shim),
			// SERVER_NAME, WAL_PATH. 2-arg: SERVER_NAME, WAL_PATH.
			// The trailing two positionals are the same in both, so
			// read from the end rather than branching on length.
			server, segPath := args[len(args)-2], args[len(args)-1]
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
