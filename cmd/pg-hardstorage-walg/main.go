// pg-hardstorage-walg — drop-in compat shim for the WAL-G CLI.
//
// Operators symlink (or rename) this binary to `wal-g` on PATH;
// existing cron jobs, archive_command lines, and monitoring
// scripts continue to work unchanged but produce native
// pg_hardstorage backups.
//
// Implementation lives in compat/walg; main is a three-line
// dispatcher.
package main

import (
	"os"

	"github.com/cybertec-postgresql/pg_hardstorage/compat/walg"
)

func main() {
	os.Exit(walg.Execute())
}
