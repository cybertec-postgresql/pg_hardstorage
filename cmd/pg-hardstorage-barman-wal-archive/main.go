// pg-hardstorage-barman-wal-archive — drop-in replacement for the
// dedicated `barman-wal-archive` companion binary.
//
// PG's archive_command invokes `barman-wal-archive <server> %p`
// during normal operation.  We mirror that boundary as a separate
// binary so symlink installs work bit-for-bit:
//
//	ln -s /usr/lib/pg_hardstorage/bin/pg-hardstorage-barman-wal-archive \
//	      /usr/bin/barman-wal-archive
package main

import (
	"fmt"
	"os"

	"github.com/cybertec-postgresql/pg_hardstorage/compat/barman"
)

func main() {
	root := barman.NewWALArchiveRoot(os.Stdout, os.Stderr)
	root.SetArgs(os.Args[1:])
	if _, err := root.ExecuteC(); err != nil {
		// Print shim-level errors (deployment-config lookup,
		// flag validation) so archive_command failures aren't
		// silent.  Errors from dispatchNative carry an empty
		// message because cli.Run already printed the
		// structured-error JSON; we don't double-print those.
		if msg := err.Error(); msg != "" {
			fmt.Fprintln(os.Stderr, msg)
		}
		os.Exit(barman.ExitCode(err))
	}
}
