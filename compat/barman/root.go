// root.go — Barman shim root: top-level `barman` cobra tree wiring the seven supported verbs.
package barman

import (
	"io"

	"github.com/spf13/cobra"
)

// NewRoot returns the top-level `barman` command tree.  Operators
// symlink bin/pg-hardstorage-barman -> /usr/bin/barman; existing
// scripts hit this root when they invoke `barman <verb>`.
//
// Seven verbs are implemented (backup, recover, list-backup,
// show-backup, check, delete, plus the wal-archive companion at
// barman-wal-archive).  Everything else refuses cleanly with a
// remediation pointing at the native equivalent.
func NewRoot(stdout, stderr io.Writer) *cobra.Command {
	root := &cobra.Command{
		Use:   "barman",
		Short: "Backup and Recovery Manager (pg_hardstorage compat shim)",
		Long: `barman is the legacy Barman CLI surface, served by the
pg_hardstorage v1.1+ compatibility shim.

This binary accepts the most-cited Barman verbs and translates
them to native pg_hardstorage commands.  Existing cron jobs,
archive_command settings, and monitoring scripts run unchanged
but produce native pg_hardstorage backups.

Verbs not in the v1.1 surface refuse with a remediation pointing
at the native equivalent.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.AddCommand(
		newBackupCmd(stdout, stderr),
		newRecoverCmd(stdout, stderr),
		newListBackupCmd(stdout, stderr),
		newShowBackupCmd(stdout, stderr),
		newCheckCmd(stdout, stderr),
		newDeleteCmd(stdout, stderr),
	)

	// Refusal-only verbs.  Each gets a tiny shell command that prints
	// the canonical refusal line and exits non-zero.  We register
	// them explicitly (rather than catch-all the unknown command)
	// because it's nicer to surface remediation at flag-parse time
	// than to fall through Cobra's "unknown command" generic error.
	root.AddCommand(
		newRefusalCmd(stderr, "cron", "use systemd timers or native `pg_hardstorage agent`"),
		newRefusalCmd(stderr, "archive-wal", "no equivalent — native uses `pg_hardstorage wal stream` (continuous)"),
		newRefusalCmd(stderr, "switch-wal", "not implemented in v1.1; native commits trigger pg_switch_wal automatically"),
		newRefusalCmd(stderr, "rebuild-xlogdb", "not implemented in v1.1; native repository is self-describing"),
		newRefusalCmd(stderr, "diagnose", "use `pg_hardstorage doctor` (more thorough)"),
		newRefusalCmd(stderr, "get-wal", "not implemented in v1.1; PG invokes `pg_hardstorage wal fetch` via restore_command"),
		newRefusalCmd(stderr, "put-wal", "not implemented in v1.1; PG invokes `barman-wal-archive` (this shim) via archive_command"),
		newRefusalCmd(stderr, "replication-status", "use `pg_hardstorage doctor <deployment>` for slot health"),
		newRefusalCmd(stderr, "show-server", "use `pg_hardstorage deployment show <deployment>`"),
		newRefusalCmd(stderr, "list-server", "use `pg_hardstorage deployment list`"),
		newRefusalCmd(stderr, "lock-directory-cleanup", "not implemented in v1.1; native uses lockless atomic commits"),
		newRefusalCmd(stderr, "verify-backup", "use `pg_hardstorage verify <deployment> <backup-id>`"),
		newRefusalCmd(stderr, "verify", "use `pg_hardstorage verify <deployment> <backup-id>`"),
		newRefusalCmd(stderr, "keep", "use `pg_hardstorage hold add <deployment> <backup-id>`"),
		newRefusalCmd(stderr, "receive-wal", "use `pg_hardstorage wal stream` (continuous, slot-based)"),
	)

	return root
}

// newRefusalCmd registers a verb that always refuses with a clear
// remediation.  Used for Barman commands that deliberately have no
// pg_hardstorage equivalent (different architecture) or that the
// v1.1 surface has not yet covered.
func newRefusalCmd(stderr io.Writer, name, suggestion string) *cobra.Command {
	c := &cobra.Command{
		Use:   name,
		Short: "Not implemented in v1.1 (Barman compat refusal)",
		// Accept any args so the user gets the remediation regardless
		// of what they typed after the verb.
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return refuseUnimplemented(stderr, name, suggestion)
		},
	}
	c.SilenceUsage = true
	c.FParseErrWhitelist.UnknownFlags = true
	return c
}
