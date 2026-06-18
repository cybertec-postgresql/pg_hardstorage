// dispatch.go — pgBackRest shim dispatcher: runs synthetic argv through internal/cli.Run; var-typed for tests.
package pgbackrest

import (
	"github.com/cybertec-postgresql/pg_hardstorage/internal/cli"
)

// dispatchNative runs the native CLI with the supplied
// synthetic argv.  Verbs build their argv via mapToNativeArgs
// + verb-specific positional args, then call this.
//
// We always go through internal/cli.Run rather than calling
// internal/backup or internal/wal directly — the shim's
// promise is "tracks the public CLI surface", not "duplicates
// the internal Go API".
//
// Returns the native CLI's process-style exit code (0 on
// success, non-zero on failure).
var dispatchNative = func(args []string) int {
	root := cli.NewRoot()
	root.SetArgs(args)
	return cli.Run(root)
}
