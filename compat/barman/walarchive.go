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
	var testOnly bool
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
			if testOnly {
				// Upstream --test checks that the Barman side is
				// reachable and configured for SERVER_NAME, and ships
				// nothing (WAL_PATH is conventionally DUMMY). The shim's
				// equivalent is reading the deployment's repository:
				// a read-only `list` fails the same way a push would
				// (missing deployment config, unreachable storage).
				native, err := injectDeploymentFlags([]string{"list", server}, server, false)
				if err != nil {
					return err
				}
				return dispatchNative(stdout, stderr, native)
			}
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
	// barman-wal-archive's documented options. They used to be
	// unregistered, so an archive_command carrying any of them died
	// with "unknown flag" and PostgreSQL retried it forever while WAL
	// piled up. The SSH-side options (-U/--user, --port, -c/--config)
	// describe the transport to the Barman host, which the shim does
	// not use; the compression switches choose what the Barman side
	// stores, and native compression is a repository setting. All are
	// accepted and have nothing to act on.
	c.Flags().BoolVarP(&testOnly, "test", "t", false,
		"test that the repository for SERVER_NAME is reachable; archive nothing")
	c.Flags().StringP("user", "U", "", "(ignored: SSH user on the Barman host)")
	c.Flags().String("port", "", "(ignored: SSH port on the Barman host)")
	c.Flags().StringP("config", "c", "", "(ignored: Barman config on the Barman host)")
	c.Flags().BoolP("gzip", "z", false, "(ignored: native compression is a repository setting)")
	c.Flags().BoolP("bzip2", "j", false, "(ignored: native compression is a repository setting)")
	for _, name := range []string{"xz", "snappy", "zstd", "lz4"} {
		c.Flags().Bool(name, false, "(ignored: native compression is a repository setting)")
	}
	c.SilenceUsage = true
	c.SilenceErrors = true
	return c
}
