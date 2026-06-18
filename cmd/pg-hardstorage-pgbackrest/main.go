// pg-hardstorage-pgbackrest — drop-in compat shim for the
// pgBackRest CLI.
//
// Operators symlink (or rename) this binary to `pgbackrest`
// on PATH; existing cron jobs, archive_command lines, and
// monitoring scripts continue to work unchanged but produce
// native pg_hardstorage backups.
//
// Implementation lives in compat/pgbackrest; main is a
// three-line dispatcher.
package main

import (
	"os"

	"github.com/cybertec-postgresql/pg_hardstorage/compat/pgbackrest"
)

func main() {
	os.Exit(pgbackrest.Execute())
}
