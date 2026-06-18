// Package barman implements the Barman drop-in replacement
// shim for pg_hardstorage v1.1.
//
// Operators running Barman today symlink bin/pg-hardstorage-barman
// into PATH as `barman` (and bin/pg-hardstorage-barman-wal-archive
// as `barman-wal-archive`).  Their existing cron jobs,
// archive_command settings, and monitoring scripts run unchanged
// but produce native pg_hardstorage backups.
//
// Architecture: every shim verb parses the Barman flags it cares
// about, builds a synthetic os.Args slice for the native CLI, and
// dispatches via internal/cli.NewRoot() + root.SetArgs(...) +
// root.Execute().  The shim never reaches into runner / repo code
// directly — coupling is the public CLI contract, so future native-
// CLI changes light up automatically.
//
// Anything outside the seven supported verbs refuses with a clear
// remediation pointing at the native equivalent.  See compat/README.md
// for the architectural rationale.
package barman
