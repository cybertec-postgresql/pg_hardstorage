// Package walg implements the WAL-G drop-in replacement
// shim for pg_hardstorage v1.1.
//
// Operators running WAL-G today symlink bin/pg-hardstorage-walg
// into PATH as `wal-g`.  Their existing cron jobs,
// archive_command settings, and monitoring scripts run unchanged
// but produce native pg_hardstorage backups.
//
// Architecture: every shim verb parses the WAL-G env vars + flags
// it cares about, builds a synthetic argv for the native CLI, and
// dispatches via internal/cli.NewRoot() + root.SetArgs(...) +
// root.Execute().  The shim never reaches into runner / repo
// code directly — coupling is the public CLI contract, so future
// native-CLI changes light up automatically.
//
// Verbs implemented (5): backup-push, backup-fetch, backup-list,
// wal-push, wal-fetch.  Anything else refuses with a clear
// remediation pointing at the native equivalent.  See
// compat/README.md for the architectural rationale.
package walg
